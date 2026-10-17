package mcp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/app/tools"
)

const defaultStartupTimeout = 15 * time.Second

type pluginStore interface {
	Plugins() ([]store.Plugin, error)
	PluginToolPreferences() ([]store.PluginToolPreference, error)
}

type ToolRuntime struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
}

type PluginRuntime struct {
	Status    string        `json:"status"`
	Tools     []ToolRuntime `json:"tools"`
	ToolCount int           `json:"toolCount"`
	LastError string        `json:"lastError,omitempty"`
}

type Manager struct {
	baseCtx  context.Context
	store    pluginStore
	registry *tools.Registry
	logger   *slog.Logger

	mu       sync.RWMutex
	sessions map[string]*sessionState
	runtime  map[string]PluginRuntime
}

// toolCallError marks an MCP application-level failure without embedding the
// server payload in logs. The original result is returned separately by CallTool.
type toolCallError struct {
	toolName string
}

func (e *toolCallError) Error() string {
	return fmt.Sprintf("mcp tool %s returned an error", e.toolName)
}

type sessionState struct {
	plugin store.Plugin
	client *stdioClient
}

func NewManager(ctx context.Context, store pluginStore, registry *tools.Registry, logger *slog.Logger) *Manager {
	if ctx == nil {
		ctx = context.Background()
	}
	if logger == nil {
		logger = slog.Default()
	}

	return &Manager{
		baseCtx:  ctx,
		store:    store,
		registry: registry,
		logger:   logger,
		sessions: make(map[string]*sessionState),
		runtime:  make(map[string]PluginRuntime),
	}
}

func (m *Manager) SyncFromStore(ctx context.Context) error {
	if m.store == nil {
		return nil
	}

	plugins, err := m.store.Plugins()
	if err != nil {
		return err
	}

	prefs, err := m.store.PluginToolPreferences()
	if err != nil {
		return err
	}

	return m.SyncPluginsWithPreferences(ctx, plugins, prefs)
}

func (m *Manager) SyncPlugins(ctx context.Context, plugins []store.Plugin) error {
	return m.SyncPluginsWithPreferences(ctx, plugins, nil)
}

func (m *Manager) SyncPluginsWithPreferences(ctx context.Context, plugins []store.Plugin, prefs []store.PluginToolPreference) error {
	if m == nil {
		return nil
	}

	preferenceIndex := indexPreferences(prefs)

	enabled := make(map[string]store.Plugin)
	seen := make(map[string]bool)
	for _, plugin := range plugins {
		if plugin.Provider != "mcp" {
			continue
		}
		seen[plugin.ID] = true
		if plugin.Enabled {
			enabled[plugin.ID] = plugin
		}
	}

	var stale []*sessionState

	m.mu.Lock()
	for pluginID, session := range m.sessions {
		if _, ok := enabled[pluginID]; !ok {
			delete(m.sessions, pluginID)
			stale = append(stale, session)
			m.registry.RemoveSource(pluginSource(pluginID))
		}
	}

	for pluginID := range m.runtime {
		if !seen[pluginID] {
			delete(m.runtime, pluginID)
		}
	}

	for _, plugin := range plugins {
		if plugin.Provider != "mcp" {
			continue
		}
		if !plugin.Enabled {
			runtime := m.runtime[plugin.ID]
			m.runtime[plugin.ID] = PluginRuntime{
				Status:    "disabled",
				Tools:     append([]ToolRuntime(nil), runtime.Tools...),
				ToolCount: runtime.ToolCount,
				LastError: runtime.LastError,
			}
		}
	}

	for pluginID, plugin := range enabled {
		if existing, ok := m.sessions[pluginID]; ok {
			delete(m.sessions, pluginID)
			stale = append(stale, existing)
		}
		m.registry.RemoveSource(pluginSource(pluginID))
		m.runtime[pluginID] = PluginRuntime{
			Status: "starting",
			Tools:  []ToolRuntime{},
		}
		_ = plugin
	}
	m.mu.Unlock()

	for _, session := range stale {
		if err := session.client.Close(); err != nil {
			m.logger.Debug("failed to close mcp session", "plugin", session.plugin.ID, "error", err)
		}
	}

	var syncErrors []string
	for _, plugin := range enabled {
		if err := m.syncPlugin(ctx, plugin, preferenceIndex[plugin.ID]); err != nil {
			syncErrors = append(syncErrors, fmt.Sprintf("%s: %v", plugin.Name, err))
		}
	}

	if len(syncErrors) > 0 {
		return errors.New(strings.Join(syncErrors, "; "))
	}

	return nil
}

func (m *Manager) CallTool(ctx context.Context, pluginID, toolName string, arguments map[string]any) (callToolResult, error) {
	session, err := m.session(pluginID)
	if err != nil {
		return callToolResult{}, err
	}

	result, err := session.client.CallTool(ctx, toolName, arguments)
	if err != nil {
		return callToolResult{}, err
	}
	if result.IsError {
		return result, &toolCallError{toolName: toolName}
	}

	return result, nil
}

func (m *Manager) Close() error {
	if m == nil {
		return nil
	}

	m.mu.Lock()
	sessions := make([]*sessionState, 0, len(m.sessions))
	for _, session := range m.sessions {
		sessions = append(sessions, session)
	}
	m.sessions = make(map[string]*sessionState)
	m.runtime = make(map[string]PluginRuntime)
	m.mu.Unlock()

	var closeErrors []string
	for _, session := range sessions {
		if err := session.client.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Sprintf("%s: %v", session.plugin.Name, err))
		}
	}
	if len(closeErrors) > 0 {
		return errors.New(strings.Join(closeErrors, "; "))
	}
	return nil
}

func (m *Manager) syncPlugin(ctx context.Context, plugin store.Plugin, prefByTool map[string]bool) error {
	cfg, err := ParsePluginConfig(plugin)
	if err != nil {
		m.setRuntime(plugin.ID, PluginRuntime{
			Status:    "error",
			Tools:     []ToolRuntime{},
			LastError: err.Error(),
		})
		return err
	}
	launchCfg, resolved := resolveInstalledFilesystemServer(cfg)
	if resolved {
		m.logger.Info("mcp filesystem using installed server", "plugin", plugin.ID, "command", launchCfg.Command, "entrypoint", launchCfg.Args[0])
	}
	m.logger.Info("mcp plugin sync starting", "plugin", plugin.ID, "transport", launchCfg.Transport, "command", launchCfg.Command, "args", launchCfg.Args)

	clientCtx, cancel := context.WithCancel(m.baseCtx)
	defer func() {
		if err != nil {
			cancel()
		}
	}()

	logger := m.logger.With("plugin", plugin.ID)
	client, err := newStdioClient(clientCtx, launchCfg, logger)
	if err != nil {
		cancel()
		err = fmt.Errorf("spawn process: %w", err)
		m.setRuntime(plugin.ID, PluginRuntime{
			Status:    "error",
			Tools:     []ToolRuntime{},
			LastError: err.Error(),
		})
		return err
	}

	handshakeCtx, release := context.WithTimeout(ctx, mcpStartupTimeout())
	defer release()

	if err = client.Initialize(handshakeCtx); err != nil {
		_ = client.Close()
		cancel()
		err = fmt.Errorf("initialize handshake: %w", err)
		m.setRuntime(plugin.ID, PluginRuntime{
			Status:    "error",
			Tools:     []ToolRuntime{},
			LastError: err.Error(),
		})
		return err
	}
	m.logger.Info("mcp plugin initialized", "plugin", plugin.ID)

	discoveryCtx, releaseDiscovery := context.WithTimeout(ctx, mcpStartupTimeout())
	defer releaseDiscovery()

	remoteTools, err := client.ListTools(discoveryCtx)
	if err != nil {
		_ = client.Close()
		cancel()
		err = fmt.Errorf("tools discovery: %w", err)
		m.setRuntime(plugin.ID, PluginRuntime{
			Status:    "error",
			Tools:     []ToolRuntime{},
			LastError: err.Error(),
		})
		return err
	}
	m.logger.Info("mcp plugin tools discovered", "plugin", plugin.ID, "tool_count", len(remoteTools))

	for _, remoteTool := range remoteTools {
		if remoteTool.Name == "directory_tree" {
			m.logger.Info("mcp directory_tree schema", "description", remoteTool.Description, "input_schema", remoteTool.InputSchema)
		}
	}

	adapters := make([]tools.Tool, 0, len(remoteTools))
	runtimeTools := make([]ToolRuntime, 0, len(remoteTools))
	for _, remoteTool := range remoteTools {
		adapter := NewToolAdapter(m, plugin, remoteTool)
		toolEnabled := true
		if disabled, ok := prefByTool[adapter.Name()]; ok {
			toolEnabled = disabled
		}
		if toolEnabled {
			adapters = append(adapters, adapter)
		}
		runtimeTools = append(runtimeTools, ToolRuntime{
			Name:        adapter.Name(),
			Description: adapter.Description(),
			Enabled:     toolEnabled,
		})
	}

	m.registry.ReplaceSource(pluginSource(plugin.ID), adapters)

	m.mu.Lock()
	m.sessions[plugin.ID] = &sessionState{
		plugin: plugin,
		client: client,
	}
	m.runtime[plugin.ID] = PluginRuntime{
		Status:    "connected",
		Tools:     runtimeTools,
		ToolCount: len(runtimeTools),
	}
	m.mu.Unlock()

	return nil
}

func mcpStartupTimeout() time.Duration {
	raw := strings.TrimSpace(os.Getenv("OLLAMA_MCP_STARTUP_TIMEOUT"))
	if raw == "" {
		return defaultStartupTimeout
	}
	if duration, err := time.ParseDuration(raw); err == nil && duration > 0 {
		return duration
	}
	return defaultStartupTimeout
}

func (m *Manager) session(pluginID string) (*sessionState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	session, ok := m.sessions[pluginID]
	if !ok {
		return nil, fmt.Errorf("mcp plugin %s is not connected", pluginID)
	}
	return session, nil
}

func pluginSource(pluginID string) string {
	return "plugin:" + pluginID
}

func (m *Manager) Snapshot() map[string]PluginRuntime {
	if m == nil {
		return nil
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	snapshot := make(map[string]PluginRuntime, len(m.runtime))
	for pluginID, runtime := range m.runtime {
		copyRuntime := runtime
		copyRuntime.Tools = append([]ToolRuntime(nil), runtime.Tools...)
		snapshot[pluginID] = copyRuntime
	}

	return snapshot
}

func (m *Manager) setRuntime(pluginID string, runtime PluginRuntime) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runtime[pluginID] = runtime
}

func indexPreferences(prefs []store.PluginToolPreference) map[string]map[string]bool {
	index := make(map[string]map[string]bool)
	for _, pref := range prefs {
		pluginPrefs := index[pref.PluginID]
		if pluginPrefs == nil {
			pluginPrefs = make(map[string]bool)
			index[pref.PluginID] = pluginPrefs
		}
		pluginPrefs[pref.ToolName] = pref.Enabled
	}
	return index
}

//go:build windows || darwin

package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/ollama/ollama/app/store"
)

const pluginProviderMCP = "mcp"

type pluginImportRequest struct {
	Config string `json:"config"`
}

type pluginUpdateRequest struct {
	Enabled bool `json:"enabled"`
}

type pluginToolUpdateRequest struct {
	Enabled bool `json:"enabled"`
}

type pluginRuntimeResponse struct {
	Status    string           `json:"status"`
	Tools     []pluginToolInfo `json:"tools"`
	ToolCount int              `json:"toolCount"`
	LastError string           `json:"lastError,omitempty"`
}

type pluginToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Enabled     bool   `json:"enabled"`
}

type pluginResponse struct {
	store.Plugin
	Runtime *pluginRuntimeResponse `json:"runtime,omitempty"`
}

func (s *Server) listPlugins(w http.ResponseWriter, _ *http.Request) error {
	if ok := s.requirePluginsEnabled(w); !ok {
		return nil
	}

	responsePlugins, err := s.pluginsResponse()
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{
		"plugins": responsePlugins,
	})
}

func (s *Server) pluginsResponse() ([]pluginResponse, error) {
	plugins, err := s.Store.Plugins()
	if err != nil {
		return nil, fmt.Errorf("failed to load plugins: %w", err)
	}

	prefs, err := s.Store.PluginToolPreferences()
	if err != nil {
		return nil, fmt.Errorf("failed to load plugin tool preferences: %w", err)
	}

	runtimeByPlugin := map[string]pluginRuntimeResponse{}
	if s.MCPManager != nil {
		for pluginID, runtime := range s.MCPManager.Snapshot() {
			tools := make([]pluginToolInfo, 0, len(runtime.Tools))
			for _, tool := range runtime.Tools {
				tools = append(tools, pluginToolInfo{
					Name:        tool.Name,
					Description: tool.Description,
					Enabled:     tool.Enabled,
				})
			}
			runtimeByPlugin[pluginID] = pluginRuntimeResponse{
				Status:    runtime.Status,
				Tools:     tools,
				ToolCount: runtime.ToolCount,
				LastError: runtime.LastError,
			}
		}
	}

	prefByPlugin := make(map[string]map[string]bool)
	for _, pref := range prefs {
		pluginPrefs := prefByPlugin[pref.PluginID]
		if pluginPrefs == nil {
			pluginPrefs = make(map[string]bool)
			prefByPlugin[pref.PluginID] = pluginPrefs
		}
		pluginPrefs[pref.ToolName] = pref.Enabled
	}

	responsePlugins := make([]pluginResponse, 0, len(plugins))
	for _, plugin := range plugins {
		item := pluginResponse{Plugin: plugin}
		if plugin.Provider == pluginProviderMCP {
			runtime := runtimeByPlugin[plugin.ID]
			runtime.Tools = mergePluginTools(runtime.Tools, prefByPlugin[plugin.ID])
			runtime.ToolCount = len(runtime.Tools)
			if !plugin.Enabled {
				runtime.Status = "disabled"
			} else if runtime.Status == "" {
				runtime.Status = "error"
			}
			item.Runtime = &runtime
		}
		responsePlugins = append(responsePlugins, item)
	}

	return responsePlugins, nil
}

func (s *Server) importPlugins(w http.ResponseWriter, r *http.Request) error {
	if ok := s.requirePluginsEnabled(w); !ok {
		return nil
	}

	var req pluginImportRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "Invalid request body.")
		return nil
	}

	plugins, err := normalizePluginImport(req.Config)
	if err != nil {
		s.writeJSONError(w, http.StatusBadRequest, err.Error())
		return nil
	}

	for _, plugin := range plugins {
		if err := s.Store.UpsertPlugin(plugin); err != nil {
			return fmt.Errorf("failed to save plugin %q: %w", plugin.Name, err)
		}
	}
	if s.MCPManager != nil {
		if err := s.MCPManager.SyncFromStore(r.Context()); err != nil {
			return fmt.Errorf("failed to sync MCP plugins: %w", err)
		}
	}

	responsePlugins, err := s.pluginsResponse()
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{
		"plugins": responsePlugins,
	})
}

func (s *Server) updatePlugin(w http.ResponseWriter, r *http.Request) error {
	if ok := s.requirePluginsEnabled(w); !ok {
		return nil
	}

	id := r.PathValue("id")
	if id == "" {
		s.writeJSONError(w, http.StatusBadRequest, "Plugin id is required.")
		return nil
	}

	var req pluginUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "Invalid request body.")
		return nil
	}

	if err := s.Store.SetPluginEnabled(id, req.Enabled); err != nil {
		s.writeJSONError(w, http.StatusNotFound, "Plugin not found.")
		return nil
	}
	if s.MCPManager != nil {
		if err := s.MCPManager.SyncFromStore(r.Context()); err != nil {
			return fmt.Errorf("failed to sync MCP plugins: %w", err)
		}
	}

	plugins, err := s.pluginsResponse()
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{
		"plugins": plugins,
	})
}

func (s *Server) updatePluginTool(w http.ResponseWriter, r *http.Request) error {
	if ok := s.requirePluginsEnabled(w); !ok {
		return nil
	}

	id := r.PathValue("id")
	toolName := r.PathValue("toolName")
	if id == "" || toolName == "" {
		s.writeJSONError(w, http.StatusBadRequest, "Plugin id and tool name are required.")
		return nil
	}

	var req pluginToolUpdateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeJSONError(w, http.StatusBadRequest, "Invalid request body.")
		return nil
	}

	if err := s.Store.SetPluginToolEnabled(id, toolName, req.Enabled); err != nil {
		s.writeJSONError(w, http.StatusNotFound, "Plugin not found.")
		return nil
	}
	if s.MCPManager != nil {
		if err := s.MCPManager.SyncFromStore(r.Context()); err != nil {
			return fmt.Errorf("failed to sync MCP plugins: %w", err)
		}
	}

	plugins, err := s.pluginsResponse()
	if err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{
		"plugins": plugins,
	})
}

func (s *Server) deletePlugin(w http.ResponseWriter, r *http.Request) error {
	if ok := s.requirePluginsEnabled(w); !ok {
		return nil
	}

	id := r.PathValue("id")
	if id == "" {
		s.writeJSONError(w, http.StatusBadRequest, "Plugin id is required.")
		return nil
	}

	if err := s.Store.DeletePlugin(id); err != nil {
		s.writeJSONError(w, http.StatusNotFound, "Plugin not found.")
		return nil
	}
	if s.MCPManager != nil {
		if err := s.MCPManager.SyncFromStore(r.Context()); err != nil {
			return fmt.Errorf("failed to sync MCP plugins: %w", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) requirePluginsEnabled(w http.ResponseWriter) bool {
	settings, err := s.Store.Settings()
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "Failed to load settings.")
		return false
	}
	if !settings.PluginsEnabled {
		s.writeJSONError(w, http.StatusForbidden, "Plugins are disabled in Settings.")
		return false
	}
	return true
}

func (s *Server) writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	})
}

func normalizePluginImport(configText string) ([]store.Plugin, error) {
	trimmed := strings.TrimSpace(configText)
	if trimmed == "" {
		return nil, fmt.Errorf("Paste a JSON configuration before saving.")
	}

	decoder := json.NewDecoder(strings.NewReader(trimmed))
	decoder.UseNumber()

	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, fmt.Errorf("Invalid JSON: %v", err)
	}
	if err := ensureDecoderEOF(decoder); err != nil {
		return nil, err
	}

	root, ok := payload.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("Expected a JSON object at the top level.")
	}

	serversValue, ok := root["mcpServers"]
	if !ok {
		serversValue = root["mcp_servers"]
	}
	if serversValue == nil {
		return nil, fmt.Errorf("Expected an object with an \"mcpServers\" section.")
	}

	servers, ok := serversValue.(map[string]any)
	if !ok || len(servers) == 0 {
		return nil, fmt.Errorf("\"mcpServers\" must be a non-empty object.")
	}

	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	plugins := make([]store.Plugin, 0, len(names))
	for _, rawName := range names {
		name := strings.TrimSpace(rawName)
		if name == "" {
			return nil, fmt.Errorf("Each MCP server must have a non-empty name.")
		}

		serverConfig, ok := servers[rawName].(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Server %q must be a JSON object.", rawName)
		}

		normalizedConfig, enabled, err := normalizeMCPServerConfig(name, serverConfig)
		if err != nil {
			return nil, err
		}

		configJSON, err := json.Marshal(normalizedConfig)
		if err != nil {
			return nil, fmt.Errorf("Failed to normalize server %q: %w", name, err)
		}

		plugins = append(plugins, store.Plugin{
			ID:       pluginProviderMCP + ":" + name,
			Name:     name,
			Provider: pluginProviderMCP,
			Enabled:  enabled,
			Config:   configJSON,
		})
	}

	return plugins, nil
}

func ensureDecoderEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return fmt.Errorf("Invalid JSON: only one top-level value is allowed.")
	}
	return nil
}

func normalizeMCPServerConfig(name string, config map[string]any) (map[string]any, bool, error) {
	normalized := make(map[string]any, len(config)+2)
	enabled := true

	for key, value := range config {
		if key == "enabled" {
			boolValue, ok := value.(bool)
			if !ok {
				return nil, false, fmt.Errorf("Server %q has an invalid \"enabled\" value; expected true or false.", name)
			}
			enabled = boolValue
			continue
		}
		normalized[key] = value
	}

	command, hasCommand := normalized["command"]
	if hasCommand {
		commandText, ok := command.(string)
		if !ok || strings.TrimSpace(commandText) == "" {
			return nil, false, fmt.Errorf("Server %q must define a non-empty string in \"command\".", name)
		}
		normalized["command"] = strings.TrimSpace(commandText)
	}

	transport, hasTransport := normalized["transport"]
	if hasTransport {
		transportText, ok := transport.(string)
		if !ok || strings.TrimSpace(transportText) == "" {
			return nil, false, fmt.Errorf("Server %q must define a non-empty string in \"transport\".", name)
		}
		normalized["transport"] = strings.TrimSpace(transportText)
	} else if hasCommand {
		normalized["transport"] = "stdio"
	}

	transportText, _ := normalized["transport"].(string)
	if transportText == "" {
		return nil, false, fmt.Errorf("Server %q must define \"transport\" or a stdio \"command\".", name)
	}

	if transportText == "stdio" {
		if !hasCommand {
			return nil, false, fmt.Errorf("Server %q must define \"command\" when using stdio transport.", name)
		}

		if args, ok := normalized["args"]; ok {
			argsList, err := normalizeStringArray(args)
			if err != nil {
				return nil, false, fmt.Errorf("Server %q has invalid \"args\": %w", name, err)
			}
			normalized["args"] = argsList
		} else {
			normalized["args"] = []string{}
		}
	}

	if env, ok := normalized["env"]; ok {
		envMap, err := normalizeStringMap(env)
		if err != nil {
			return nil, false, fmt.Errorf("Server %q has invalid \"env\": %w", name, err)
		}
		normalized["env"] = envMap
	}

	return normalized, enabled, nil
}

func normalizeStringArray(value any) ([]string, error) {
	rawArgs, ok := value.([]any)
	if !ok {
		if typedArgs, ok := value.([]string); ok {
			return typedArgs, nil
		}
		return nil, fmt.Errorf("expected an array of strings")
	}

	args := make([]string, 0, len(rawArgs))
	for _, item := range rawArgs {
		text, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("expected an array of strings")
		}
		args = append(args, text)
	}
	return args, nil
}

func normalizeStringMap(value any) (map[string]string, error) {
	rawEnv, ok := value.(map[string]any)
	if !ok {
		if typedEnv, ok := value.(map[string]string); ok {
			return typedEnv, nil
		}
		return nil, fmt.Errorf("expected an object with string values")
	}

	env := make(map[string]string, len(rawEnv))
	for key, rawValue := range rawEnv {
		text, ok := rawValue.(string)
		if !ok {
			return nil, fmt.Errorf("expected an object with string values")
		}
		env[key] = text
	}
	return env, nil
}

func mergePluginTools(runtimeTools []pluginToolInfo, prefs map[string]bool) []pluginToolInfo {
	tools := make([]pluginToolInfo, 0, len(runtimeTools)+len(prefs))
	seen := make(map[string]bool, len(runtimeTools))
	for _, tool := range runtimeTools {
		if enabled, ok := prefs[tool.Name]; ok {
			tool.Enabled = enabled
		}
		tools = append(tools, tool)
		seen[tool.Name] = true
	}

	for name, enabled := range prefs {
		if seen[name] {
			continue
		}
		tools = append(tools, pluginToolInfo{
			Name:    name,
			Enabled: enabled,
		})
	}

	sort.Slice(tools, func(i, j int) bool {
		return tools[i].Name < tools[j].Name
	})

	return tools
}

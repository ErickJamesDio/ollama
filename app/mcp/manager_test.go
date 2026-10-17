package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/app/tools"
)

func TestInitializeParamsJSONIncludesRequiredFields(t *testing.T) {
	payload, err := json.Marshal(initializeParams{
		ProtocolVersion: protocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo: clientInfo{
			Name:    "ollama-desktop",
			Version: "test-version",
		},
	})
	if err != nil {
		t.Fatalf("Marshal(initializeParams) error = %v", err)
	}

	var decoded map[string]any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("Unmarshal payload error = %v", err)
	}

	if decoded["protocolVersion"] != protocolVersion {
		t.Fatalf("protocolVersion = %#v, want %#v", decoded["protocolVersion"], protocolVersion)
	}

	capabilities, ok := decoded["capabilities"].(map[string]any)
	if !ok {
		t.Fatalf("capabilities missing or wrong type: %#v", decoded["capabilities"])
	}
	if len(capabilities) != 0 {
		t.Fatalf("capabilities = %#v, want empty object", capabilities)
	}

	clientInfoValue, ok := decoded["clientInfo"].(map[string]any)
	if !ok {
		t.Fatalf("clientInfo missing or wrong type: %#v", decoded["clientInfo"])
	}
	if clientInfoValue["name"] != "ollama-desktop" {
		t.Fatalf("clientInfo.name = %#v, want %#v", clientInfoValue["name"], "ollama-desktop")
	}
	if clientInfoValue["version"] != "test-version" {
		t.Fatalf("clientInfo.version = %#v, want %#v", clientInfoValue["version"], "test-version")
	}
}

func TestManagerSyncPluginsRegistersAndExecutesTools(t *testing.T) {
	registry := tools.NewRegistry()
	manager := NewManager(t.Context(), nil, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer manager.Close()

	plugin := helperPlugin(t, "example")
	if err := manager.SyncPlugins(t.Context(), []store.Plugin{plugin}); err != nil {
		t.Fatalf("SyncPlugins() error = %v", err)
	}

	toolName := "mcp.example.echo"
	if _, ok := registry.Get(toolName); !ok {
		t.Fatalf("expected %q to be registered", toolName)
	}

	result, text, err := registry.Execute(t.Context(), toolName, map[string]any{"message": "hello"})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if text != "echo: hello" {
		t.Fatalf("Execute() text = %q, want %q", text, "echo: hello")
	}
	if result == nil {
		t.Fatal("expected Execute() to return a structured result")
	}
}

func TestManagerPreservesMCPErrorResult(t *testing.T) {
	registry := tools.NewRegistry()
	manager := NewManager(t.Context(), nil, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer manager.Close()

	plugin := helperPlugin(t, "example")
	if err := manager.SyncPlugins(t.Context(), []store.Plugin{plugin}); err != nil {
		t.Fatalf("SyncPlugins() error = %v", err)
	}

	result, text, err := registry.Execute(t.Context(), "mcp.example.echo", map[string]any{"message": "fail"})
	if err == nil {
		t.Fatal("Execute() error = nil, want MCP application error")
	}
	if !strings.Contains(text, `"exitCode":7`) || !strings.Contains(text, `"stderr":"build failed"`) {
		t.Fatalf("Execute() text did not preserve diagnostics: %q", text)
	}
	raw, ok := result.(*json.RawMessage)
	if !ok || raw == nil {
		t.Fatalf("Execute() result = %T, want *json.RawMessage", result)
	}
	var decoded callToolResult
	if err := json.Unmarshal(*raw, &decoded); err != nil {
		t.Fatalf("decode preserved result: %v", err)
	}
	if !decoded.IsError {
		t.Fatal("preserved result lost isError=true")
	}
}

func TestManagerSyncPluginsRemovesDisabledOrDeletedSources(t *testing.T) {
	registry := tools.NewRegistry()
	manager := NewManager(t.Context(), nil, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer manager.Close()

	pluginOne := helperPlugin(t, "one")
	pluginTwo := helperPlugin(t, "two")
	if err := manager.SyncPlugins(t.Context(), []store.Plugin{pluginOne, pluginTwo}); err != nil {
		t.Fatalf("initial SyncPlugins() error = %v", err)
	}

	if err := manager.SyncPlugins(t.Context(), []store.Plugin{pluginOne}); err != nil {
		t.Fatalf("resync SyncPlugins() error = %v", err)
	}

	if _, ok := registry.Get("mcp.one.echo"); !ok {
		t.Fatal("expected source one tool to remain")
	}
	if _, ok := registry.Get("mcp.two.echo"); ok {
		t.Fatal("expected source two tool to be removed")
	}
}

func TestManagerNamespacesCollidingToolNamesPerPlugin(t *testing.T) {
	registry := tools.NewRegistry()
	manager := NewManager(t.Context(), nil, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer manager.Close()

	pluginOne := helperPlugin(t, "alpha")
	pluginTwo := helperPlugin(t, "beta")
	if err := manager.SyncPlugins(t.Context(), []store.Plugin{pluginOne, pluginTwo}); err != nil {
		t.Fatalf("SyncPlugins() error = %v", err)
	}

	if _, ok := registry.Get("mcp.alpha.echo"); !ok {
		t.Fatal("expected alpha namespaced tool to be registered")
	}
	if _, ok := registry.Get("mcp.beta.echo"); !ok {
		t.Fatal("expected beta namespaced tool to be registered")
	}
}

func TestManagerSyncPluginsRespectsToolPreferences(t *testing.T) {
	registry := tools.NewRegistry()
	manager := NewManager(t.Context(), nil, registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	defer manager.Close()

	plugin := helperPlugin(t, "example")
	prefs := []store.PluginToolPreference{
		{
			PluginID: plugin.ID,
			ToolName: "mcp.example.echo",
			Enabled:  false,
		},
	}

	if err := manager.SyncPluginsWithPreferences(t.Context(), []store.Plugin{plugin}, prefs); err != nil {
		t.Fatalf("SyncPluginsWithPreferences() error = %v", err)
	}

	if _, ok := registry.Get("mcp.example.echo"); ok {
		t.Fatal("expected disabled tool to stay out of the registry")
	}

	snapshot := manager.Snapshot()
	runtime := snapshot[plugin.ID]
	if len(runtime.Tools) != 1 {
		t.Fatalf("runtime tools = %d, want 1", len(runtime.Tools))
	}
	if runtime.Tools[0].Enabled {
		t.Fatal("expected runtime tool to be marked disabled")
	}
}

func TestHelperProcessMCPServer(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER") != "1" {
		return
	}

	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)
	fmt.Fprintln(os.Stderr, "helper ready")

	msg, err := readEnvelope(reader)
	if err != nil {
		os.Exit(2)
	}
	if msg.Method != "initialize" {
		fmt.Fprintf(os.Stderr, "unexpected first method %q\n", msg.Method)
		os.Exit(3)
	}
	writeHelperResponse(writer, msg.ID, map[string]any{
		"protocolVersion": protocolVersion,
		"serverInfo": map[string]any{
			"name":    "helper",
			"version": "1.0.0",
		},
		"capabilities": map[string]any{},
	})

	msg, err = readEnvelope(reader)
	if err != nil {
		os.Exit(4)
	}
	if msg.Method != "notifications/initialized" {
		fmt.Fprintf(os.Stderr, "unexpected notification %q\n", msg.Method)
		os.Exit(5)
	}

	msg, err = readEnvelope(reader)
	if err != nil {
		os.Exit(6)
	}
	if msg.Method != "tools/list" {
		fmt.Fprintf(os.Stderr, "unexpected discovery method %q\n", msg.Method)
		os.Exit(7)
	}
	writeHelperResponse(writer, msg.ID, map[string]any{
		"tools": []map[string]any{
			{
				"name":        "echo",
				"description": "Echo a message",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"message": map[string]any{
							"type": "string",
						},
					},
					"required": []string{"message"},
				},
			},
		},
	})

	for {
		msg, err = readEnvelope(reader)
		if err != nil {
			if err == io.EOF || strings.Contains(err.Error(), "file already closed") {
				return
			}
			os.Exit(8)
		}
		if msg.Method != "tools/call" {
			continue
		}

		var params callToolParams
		if err := json.Unmarshal(msg.Params, &params); err != nil {
			os.Exit(9)
		}
		message, _ := params.Arguments["message"].(string)
		if message == "fail" {
			writeHelperResponse(writer, msg.ID, map[string]any{
				"content": []map[string]any{{
					"type": "text",
					"text": `{"success":false,"exitCode":7,"stdout":"partial output","stderr":"build failed"}`,
				}},
				"structuredContent": map[string]any{
					"success":  false,
					"exitCode": 7,
					"stdout":   "partial output",
					"stderr":   "build failed",
				},
				"isError": true,
			})
			continue
		}
		writeHelperResponse(writer, msg.ID, map[string]any{
			"content": []map[string]any{
				{
					"type": "text",
					"text": "echo: " + message,
				},
			},
			"structuredContent": map[string]any{
				"message": message,
			},
		})
	}
}

func helperPlugin(t *testing.T, name string) store.Plugin {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}

	cfg := map[string]any{
		"command":   exe,
		"args":      []string{"-test.run=TestHelperProcessMCPServer", "--", "mcp-helper"},
		"transport": "stdio",
		"env": map[string]string{
			"GO_WANT_MCP_HELPER": "1",
		},
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal helper plugin config: %v", err)
	}

	return store.Plugin{
		ID:       "mcp:" + name,
		Name:     name,
		Provider: "mcp",
		Enabled:  true,
		Config:   raw,
	}
}

func writeHelperResponse(writer *bufio.Writer, id json.RawMessage, result any) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(id),
		"result":  result,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		os.Exit(10)
	}
	body = append(body, '\n')
	if _, err := writer.Write(body); err != nil {
		os.Exit(11)
	}
	if err := writer.Flush(); err != nil {
		os.Exit(12)
	}
}

func TestStdioClientInitializeSequence(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cfgPlugin := helperPlugin(t, "sequence")
	cfg, err := ParsePluginConfig(cfgPlugin)
	if err != nil {
		t.Fatalf("ParsePluginConfig() error = %v", err)
	}

	client, err := newStdioClient(ctx, cfg, logger)
	if err != nil {
		t.Fatalf("newStdioClient() error = %v", err)
	}
	defer client.Close()

	if err := client.Initialize(ctx); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	tools, err := client.ListTools(ctx)
	if err != nil {
		t.Fatalf("ListTools() error = %v", err)
	}
	if len(tools) != 1 || tools[0].Name != "echo" {
		t.Fatalf("ListTools() = %+v, want single echo tool", tools)
	}
}

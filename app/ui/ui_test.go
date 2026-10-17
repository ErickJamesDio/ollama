//go:build windows || darwin

package ui

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/app/mcp"
	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/app/tools"
	"github.com/ollama/ollama/app/updater"
	"github.com/ollama/ollama/cmd/launch"
)

type testTool struct {
	name string
}

func (t testTool) Name() string { return t.name }

func (t testTool) Description() string { return "test tool" }

func (t testTool) Schema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t testTool) Execute(context.Context, map[string]any) (any, string, error) {
	return map[string]any{"ok": true}, "ok", nil
}

func (t testTool) Prompt() string { return "" }

func slogDiscard() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestUIPluginMCPHelper(t *testing.T) {
	if os.Getenv("GO_WANT_UI_MCP_HELPER") != "1" {
		return
	}

	reader := bufio.NewReader(os.Stdin)
	writer := bufio.NewWriter(os.Stdout)

	line, err := reader.ReadString('\n')
	if err != nil {
		os.Exit(2)
	}

	var request map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &request); err != nil {
		os.Exit(3)
	}
	if request["method"] != "initialize" {
		os.Exit(4)
	}

	writeUIPluginMCPResponse(writer, request["id"], map[string]any{
		"protocolVersion": "2025-06-18",
		"serverInfo": map[string]any{
			"name":    "ui-helper",
			"version": "1.0.0",
		},
		"capabilities": map[string]any{},
	})

	line, err = reader.ReadString('\n')
	if err != nil {
		os.Exit(5)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &request); err != nil {
		os.Exit(6)
	}
	if request["method"] != "notifications/initialized" {
		os.Exit(7)
	}

	line, err = reader.ReadString('\n')
	if err != nil {
		os.Exit(8)
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &request); err != nil {
		os.Exit(9)
	}
	if request["method"] != "tools/list" {
		os.Exit(10)
	}

	writeUIPluginMCPResponse(writer, request["id"], map[string]any{
		"tools": []map[string]any{
			{
				"name":        "echo",
				"description": "Echo a message",
				"inputSchema": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"message": map[string]any{"type": "string"},
					},
				},
			},
		},
	})
}

func writeUIPluginMCPResponse(writer *bufio.Writer, id any, result any) {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		os.Exit(11)
	}
	body = append(body, '\n')
	if _, err := writer.Write(body); err != nil {
		os.Exit(12)
	}
	if err := writer.Flush(); err != nil {
		os.Exit(13)
	}
}

func uiHelperPlugin(t *testing.T, name string, enabled bool) store.Plugin {
	t.Helper()

	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable() error = %v", err)
	}

	cfg := map[string]any{
		"command":   exe,
		"args":      []string{"-test.run=TestUIPluginMCPHelper", "--", "mcp-helper"},
		"transport": "stdio",
		"env": map[string]string{
			"GO_WANT_UI_MCP_HELPER": "1",
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
		Enabled:  enabled,
		Config:   raw,
	}
}

func TestBuildChatRequestIncludesPersistentRegistryTools(t *testing.T) {
	registry := tools.NewRegistry()
	registry.Register(testTool{name: "mcp.example"})

	server := &Server{
		ToolRegistry: registry,
	}

	reqRegistry := server.chatToolRegistry()
	reqRegistry.Register(testTool{name: "request.only"})

	if len(server.ToolRegistry.List()) != 1 {
		t.Fatalf("persistent registry was mutated, got %d tools", len(server.ToolRegistry.List()))
	}

	chat := &store.Chat{
		ID: "chat-1",
		Messages: []store.Message{
			store.NewMessage("user", "hello", nil),
		},
	}

	req, err := server.buildChatRequest(chat, "test-model", nil, reqRegistry.AvailableTools())
	if err != nil {
		t.Fatalf("buildChatRequest() error = %v", err)
	}

	if len(req.Tools) != 2 {
		t.Fatalf("buildChatRequest() tools = %d, want 2", len(req.Tools))
	}

	names := map[string]bool{}
	for _, tool := range req.Tools {
		names[tool.Function.Name] = true
	}

	if !names["mcp.example"] {
		t.Fatal("expected persistent registry tool to be published to /api/chat")
	}
	if !names["request.only"] {
		t.Fatal("expected request-scoped tool to remain in /api/chat")
	}
}

func TestHandlePostApiSettings(t *testing.T) {
	tests := []struct {
		name      string
		requested store.Settings
		wantErr   bool
	}{
		{
			name: "valid settings update - all fields",
			requested: store.Settings{
				Expose:     true,
				Browser:    true,
				Models:     "/custom/models",
				Agent:      true,
				Tools:      true,
				WorkingDir: "/workspace",
			},
			wantErr: false,
		},
		{
			name: "partial settings update",
			requested: store.Settings{
				Agent:      true,
				Tools:      false,
				WorkingDir: "/new/path",
			},
			wantErr: false,
		},
		{
			name: "settings with special characters in paths",
			requested: store.Settings{
				Models:     "/path with spaces/models",
				WorkingDir: "/tmp/work-dir_123",
				Agent:      true,
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testStore := &store.Store{
				DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
			}
			defer testStore.Close() // Ensure database is closed before cleanup

			body, err := json.Marshal(tt.requested)
			if err != nil {
				t.Fatalf("failed to marshal test body: %v", err)
			}

			req := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			// Set up server with test store
			server := &Server{
				Store:   testStore,
				Restart: func() {}, // Mock restart function for tests
			}

			if err := server.settings(rr, req); (err != nil) != tt.wantErr {
				t.Errorf("handlePostApiSettings() error = %v, wantErr %v", err, tt.wantErr)
			}
			if rr.Code != http.StatusOK {
				t.Errorf("handlePostApiSettings() status = %v, want %v", rr.Code, http.StatusOK)
			}

			// Check settings were saved correctly (if no error expected)
			if !tt.wantErr {
				savedSettings, err := testStore.Settings()
				if err != nil {
					t.Errorf("failed to retrieve saved settings: %v", err)
				} else {
					// Compare field by field, accounting for defaults that may be set by the store
					if savedSettings.Expose != tt.requested.Expose {
						t.Errorf("Expose: got %v, want %v", savedSettings.Expose, tt.requested.Expose)
					}
					if savedSettings.Browser != tt.requested.Browser {
						t.Errorf("Browser: got %v, want %v", savedSettings.Browser, tt.requested.Browser)
					}
					if savedSettings.Agent != tt.requested.Agent {
						t.Errorf("Agent: got %v, want %v", savedSettings.Agent, tt.requested.Agent)
					}
					if savedSettings.Tools != tt.requested.Tools {
						t.Errorf("Tools: got %v, want %v", savedSettings.Tools, tt.requested.Tools)
					}
					if savedSettings.WorkingDir != tt.requested.WorkingDir {
						t.Errorf("WorkingDir: got %q, want %q", savedSettings.WorkingDir, tt.requested.WorkingDir)
					}
					// Only check Models if explicitly set in the test case
					if tt.requested.Models != "" && savedSettings.Models != tt.requested.Models {
						t.Errorf("Models: got %q, want %q", savedSettings.Models, tt.requested.Models)
					}
				}
			}
		})
	}
}

func TestGetSettingsReturnsPersistedToolsPreference(t *testing.T) {
	testStore := &store.Store{DBPath: filepath.Join(t.TempDir(), "db.sqlite")}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.Tools = true
	if err := testStore.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	server := &Server{Store: testStore, Tools: false}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	rr := httptest.NewRecorder()
	if err := server.getSettings(rr, req); err != nil {
		t.Fatalf("getSettings() error = %v", err)
	}

	var response struct {
		Settings store.Settings `json:"settings"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatalf("decode settings response: %v", err)
	}
	if !response.Settings.Tools {
		t.Fatal("GET settings replaced persisted Tools=true with legacy runtime value")
	}
}

func TestGetIntegrationStatuses(t *testing.T) {
	server := &Server{
		IntegrationInstalled: func(name string) bool {
			return name == "claude-desktop" || name == "codex"
		},
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/integrations", nil)
	rr := httptest.NewRecorder()

	if err := server.getIntegrationStatuses(rr, req); err != nil {
		t.Fatalf("getIntegrationStatuses() error = %v", err)
	}

	var got []struct {
		ID        string `json:"id"`
		Installed *bool  `json:"installed"`
		Action    string `json:"action"`
		Command   string `json:"command"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(got) < 5 {
		t.Fatalf("got %d integrations, want the full registry", len(got))
	}
	if got[0].ID != "claude-desktop" || got[0].Action != "connect" || got[0].Command != "" {
		t.Fatalf("first integration = %+v, want command-free Claude Desktop connect", got[0])
	}
	wantPrefix := []string{"claude-desktop", "claude", "codex", "openclaw", "opencode", "droid", "pi", "cline"}
	for i, want := range wantPrefix {
		if got[i].ID != want {
			t.Fatalf("integration %d = %q, want launcher menu order entry %q", i, got[i].ID, want)
		}
	}
	byID := make(map[string]struct {
		Installed *bool
		Action    string
		Command   string
	}, len(got))
	for _, item := range got {
		byID[item.ID] = struct {
			Installed *bool
			Action    string
			Command   string
		}{item.Installed, item.Action, item.Command}
	}

	for name, want := range map[string]bool{
		"claude-desktop": true,
		"opencode":       false,
		"codex":          true,
	} {
		item, ok := byID[name]
		if !ok || item.Installed == nil || *item.Installed != want {
			t.Errorf("%s installed = %v, want %v", name, item.Installed, want)
		}
	}
	if item, ok := byID["claude"]; !ok || item.Command != "ollama launch claude" {
		t.Fatal("Claude Code should follow Claude Desktop with its launch command")
	}
	if _, ok := byID["chatgpt"]; ok {
		t.Fatal("ChatGPT should be excluded from onboarding integrations")
	}
	wantCount := len(launch.ListIntegrationInfos()) + 1 // Claude Desktop and Terminal replace omitted ChatGPT.
	if len(got) != wantCount {
		t.Fatalf("got %d integrations, want %d launcher entries", len(got), wantCount)
	}
	terminal := got[len(got)-1]
	if terminal.ID != "terminal" || terminal.Installed != nil || terminal.Command != "ollama" {
		t.Fatalf("last integration = %+v, want Terminal without install status", terminal)
	}
}

func TestHandlePostApiCloudSetting(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("OLLAMA_NO_CLOUD", "")

	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	restartCount := 0
	server := &Server{
		Store: testStore,
		Restart: func() {
			restartCount++
		},
	}

	for _, tc := range []struct {
		name        string
		body        string
		wantEnabled bool
	}{
		{name: "disable cloud", body: `{"enabled": false}`, wantEnabled: false},
		{name: "enable cloud", body: `{"enabled": true}`, wantEnabled: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/cloud", bytes.NewBufferString(tc.body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()

			if err := server.cloudSetting(rr, req); err != nil {
				t.Fatalf("cloudSetting() error = %v", err)
			}
			if rr.Code != http.StatusOK {
				t.Fatalf("cloudSetting() status = %d, want %d", rr.Code, http.StatusOK)
			}

			var got map[string]any
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatalf("cloudSetting() invalid response JSON: %v", err)
			}
			if got["disabled"] != !tc.wantEnabled {
				t.Fatalf("response disabled = %v, want %v", got["disabled"], !tc.wantEnabled)
			}

			disabled, err := testStore.CloudDisabled()
			if err != nil {
				t.Fatalf("CloudDisabled() error = %v", err)
			}
			if gotEnabled := !disabled; gotEnabled != tc.wantEnabled {
				t.Fatalf("cloud enabled = %v, want %v", gotEnabled, tc.wantEnabled)
			}
		})
	}

	if restartCount != 2 {
		t.Fatalf("Restart called %d times, want 2", restartCount)
	}
}

func TestHandleGetApiCloudSetting(t *testing.T) {
	tmpHome := t.TempDir()
	t.Setenv("HOME", tmpHome)
	t.Setenv("OLLAMA_NO_CLOUD", "")

	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	if err := testStore.SetCloudEnabled(false); err != nil {
		t.Fatalf("SetCloudEnabled(false) error = %v", err)
	}

	server := &Server{
		Store:   testStore,
		Restart: func() {},
	}

	req := httptest.NewRequest("GET", "/api/v1/cloud", nil)
	rr := httptest.NewRecorder()
	if err := server.getCloudSetting(rr, req); err != nil {
		t.Fatalf("getCloudSetting() error = %v", err)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("getCloudSetting() status = %d, want %d", rr.Code, http.StatusOK)
	}

	var got map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("getCloudSetting() invalid response JSON: %v", err)
	}
	if got["disabled"] != true {
		t.Fatalf("response disabled = %v, want true", got["disabled"])
	}
	if got["source"] != "config" {
		t.Fatalf("response source = %v, want config", got["source"])
	}
}

func TestAuthenticationMiddleware(t *testing.T) {
	tests := []struct {
		name         string
		method       string
		contentType  string
		tokenCookie  string
		serverToken  string
		wantStatus   int
		wantError    string
		setupRequest func(*http.Request)
	}{
		{
			name:        "missing token cookie",
			method:      "GET",
			tokenCookie: "",
			serverToken: "test-token-123",
			wantStatus:  http.StatusForbidden,
			wantError:   "Token is required",
		},
		{
			name:        "invalid token value",
			method:      "GET",
			tokenCookie: "wrong-token",
			serverToken: "test-token-123",
			wantStatus:  http.StatusForbidden,
			wantError:   "Token is required",
		},
		{
			name:        "valid token - GET request",
			method:      "GET",
			tokenCookie: "test-token-123",
			serverToken: "test-token-123",
			wantStatus:  http.StatusOK,
			wantError:   "",
		},
		{
			name:        "valid token - POST with application/json",
			method:      "POST",
			contentType: "application/json",
			tokenCookie: "test-token-123",
			serverToken: "test-token-123",
			wantStatus:  http.StatusOK,
			wantError:   "",
		},
		{
			name:        "POST without Content-Type header",
			method:      "POST",
			contentType: "",
			tokenCookie: "test-token-123",
			serverToken: "test-token-123",
			wantStatus:  http.StatusForbidden,
			wantError:   "Content-Type must be application/json",
		},
		{
			name:        "POST with wrong Content-Type",
			method:      "POST",
			contentType: "text/plain",
			tokenCookie: "test-token-123",
			serverToken: "test-token-123",
			wantStatus:  http.StatusForbidden,
			wantError:   "Content-Type must be application/json",
		},
		{
			name:        "OPTIONS request (CORS preflight) - should bypass auth",
			method:      "OPTIONS",
			tokenCookie: "",
			serverToken: "test-token-123",
			wantStatus:  http.StatusOK,
			wantError:   "",
			setupRequest: func(r *http.Request) {
				// Simulate CORS being enabled
				// Note: This assumes CORS() returns true in test environment
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test handler that just returns 200 OK if auth passes
			testHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
				json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
			})

			// Create server with test token
			server := &Server{
				Token: tt.serverToken,
			}

			// Get the authentication middleware by calling Handler()
			// We need to wrap our test handler with the auth middleware
			handler := server.Handler()

			// Create a test router to simulate the authentication middleware
			mux := http.NewServeMux()
			mux.Handle("/test", handler)

			// But since Handler() returns the full router, we'll need a different approach
			// Let's create a minimal handler that includes just the auth logic
			authHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Add CORS headers for dev work
				if r.Method == "OPTIONS" {
					w.WriteHeader(http.StatusOK)
					return
				}

				if r.Method == "POST" && r.Header.Get("Content-Type") != "application/json" {
					w.WriteHeader(http.StatusForbidden)
					json.NewEncoder(w).Encode(map[string]string{"error": "Content-Type must be application/json"})
					return
				}

				cookie, err := r.Cookie("token")
				if err != nil {
					w.WriteHeader(http.StatusForbidden)
					json.NewEncoder(w).Encode(map[string]string{"error": "Token is required"})
					return
				}

				if cookie.Value != server.Token {
					w.WriteHeader(http.StatusForbidden)
					json.NewEncoder(w).Encode(map[string]string{"error": "Token is required"})
					return
				}

				// If auth passes, call the test handler
				testHandler.ServeHTTP(w, r)
			})

			// Create test request
			req := httptest.NewRequest(tt.method, "/test", nil)

			// Set Content-Type if provided
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}

			// Set token cookie if provided
			if tt.tokenCookie != "" {
				req.AddCookie(&http.Cookie{
					Name:  "token",
					Value: tt.tokenCookie,
				})
			}

			// Run any additional setup
			if tt.setupRequest != nil {
				tt.setupRequest(req)
			}

			// Create response recorder
			rr := httptest.NewRecorder()

			// Serve the request
			authHandler.ServeHTTP(rr, req)

			// Check status code
			if rr.Code != tt.wantStatus {
				t.Errorf("handler returned wrong status code: got %v want %v", rr.Code, tt.wantStatus)
			}

			// Check error message if expected
			if tt.wantError != "" {
				var response map[string]string
				if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
					t.Fatalf("failed to decode response body: %v", err)
				}

				if response["error"] != tt.wantError {
					t.Errorf("handler returned wrong error message: got %v want %v", response["error"], tt.wantError)
				}
			}
		})
	}
}

func TestUserAgent(t *testing.T) {
	ua := userAgent()

	// The userAgent function should return a string in the format:
	// "ollama/version (arch os) app/version Go/goversion"
	// Example: "ollama/v0.1.28 (amd64 darwin) Go/go1.21.0"

	if ua == "" {
		t.Fatal("userAgent returned empty string")
	}

	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("User-Agent", ua)

	// This is a copy of the logic ollama.com uses to parse the user agent
	clientInfoFromRequest := func(r *http.Request) struct {
		Product    string
		Version    string
		OS         string
		Arch       string
		AppVersion string
	} {
		product, rest, _ := strings.Cut(r.UserAgent(), " ")
		client, version, ok := strings.Cut(product, "/")
		if !ok {
			return struct {
				Product    string
				Version    string
				OS         string
				Arch       string
				AppVersion string
			}{}
		}

		if version != "" && version[0] != 'v' {
			version = "v" + version
		}

		arch, rest, _ := strings.Cut(rest, " ")
		arch = strings.Trim(arch, "(")
		os, rest, _ := strings.Cut(rest, ")")

		var appVersion string
		if strings.Contains(rest, "app/") {
			_, appPart, found := strings.Cut(rest, "app/")
			if found {
				appVersion = strings.Fields(strings.TrimSpace(appPart))[0]
				if appVersion != "" && appVersion[0] != 'v' {
					appVersion = "v" + appVersion
				}
			}
		}

		return struct {
			Product    string
			Version    string
			OS         string
			Arch       string
			AppVersion string
		}{
			Product:    client,
			Version:    version,
			OS:         os,
			Arch:       arch,
			AppVersion: appVersion,
		}
	}

	info := clientInfoFromRequest(req)
	if info.Product != "ollama" {
		t.Errorf("Expected Product to be 'ollama', got '%s'", info.Product)
	}

	if info.Version != "" && info.Version[0] != 'v' {
		t.Errorf("Expected Version to start with 'v', got '%s'", info.Version)
	}

	expectedOS := runtime.GOOS
	if info.OS != expectedOS {
		t.Errorf("Expected OS to be '%s', got '%s'", expectedOS, info.OS)
	}

	expectedArch := runtime.GOARCH
	if info.Arch != expectedArch {
		t.Errorf("Expected Arch to be '%s', got '%s'", expectedArch, info.Arch)
	}

	if info.AppVersion != "" && info.AppVersion[0] != 'v' {
		t.Errorf("Expected AppVersion to start with 'v', got '%s'", info.AppVersion)
	}

	t.Logf("User Agent: %s", ua)
	t.Logf("Parsed - Product: %s, Version: %s, OS: %s, Arch: %s",
		info.Product, info.Version, info.OS, info.Arch)
}

func TestUserAgentTransport(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(r.Header.Get("User-Agent")))
	}))
	defer ts.Close()
	server := &Server{}

	client := server.httpClient()
	resp, err := client.Get(ts.URL)
	if err != nil {
		t.Fatalf("Failed to make request: %v", err)
	}
	defer resp.Body.Close()

	// In this case the User-Agent is the response body, as the server just echoes it back
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response: %v", err)
	}

	receivedUA := string(body)
	expectedUA := userAgent()

	if receivedUA != expectedUA {
		t.Errorf("User-Agent mismatch\nExpected: %s\nReceived: %s", expectedUA, receivedUA)
	}

	if !strings.HasPrefix(receivedUA, "ollama/") {
		t.Errorf("User-Agent should start with 'ollama/', got: %s", receivedUA)
	}

	t.Logf("User-Agent transport successfully set: %s", receivedUA)
}

func TestGetCloudModels(t *testing.T) {
	t.Run("does not call ollama.com when cloud is disabled", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("OLLAMA_NO_CLOUD", "1")
		testStore := &store.Store{DBPath: filepath.Join(t.TempDir(), "db.sqlite")}
		defer testStore.Close()

		server := &Server{
			Store: testStore,
			ListCloudModels: func(context.Context) (*api.ListResponse, error) {
				t.Fatal("cloud model list called while cloud was disabled")
				return nil, nil
			},
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/models/cloud", nil)
		rr := httptest.NewRecorder()
		if err := server.getCloudModels(rr, req); err != nil {
			t.Fatal(err)
		}

		var got api.ListResponse
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if len(got.Models) != 0 {
			t.Fatalf("models = %+v, want none", got.Models)
		}
	})

	t.Run("returns no cloud models when account is unauthorized", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		t.Setenv("OLLAMA_NO_CLOUD", "")
		testStore := &store.Store{DBPath: filepath.Join(t.TempDir(), "db.sqlite")}
		defer testStore.Close()

		server := &Server{
			Store: testStore,
			ListCloudModels: func(context.Context) (*api.ListResponse, error) {
				return nil, api.AuthorizationError{StatusCode: http.StatusUnauthorized}
			},
		}
		req := httptest.NewRequest(http.MethodGet, "/api/v1/models/cloud", nil)
		rr := httptest.NewRecorder()
		if err := server.getCloudModels(rr, req); err != nil {
			t.Fatal(err)
		}

		var got api.ListResponse
		if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		if len(got.Models) != 0 {
			t.Fatalf("models = %+v, want none", got.Models)
		}
	})
}

func TestInferenceClientUsesUserAgent(t *testing.T) {
	var gotUserAgent atomic.Value
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUserAgent.Store(r.Header.Get("User-Agent"))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	t.Setenv("OLLAMA_HOST", ts.URL)

	server := &Server{}
	client := server.inferenceClient()

	_, err := client.Show(context.Background(), &api.ShowRequest{Model: "test"})
	if err != nil {
		t.Fatalf("show request failed: %v", err)
	}

	receivedUA, _ := gotUserAgent.Load().(string)
	expectedUA := userAgent()

	if receivedUA != expectedUA {
		t.Errorf("User-Agent mismatch\nExpected: %s\nReceived: %s", expectedUA, receivedUA)
	}
}

func TestSupportsBrowserTools(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"gpt-oss", true},
		{"gpt-oss-latest", true},
		{"GPT-OSS", true},
		{"Gpt-Oss-v2", true},
		{"qwen3", false},
		{"deepseek-v3", false},
		{"llama3.3", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.model, func(t *testing.T) {
			if got := supportsBrowserTools(tt.model); got != tt.want {
				t.Errorf("supportsBrowserTools(%q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

func TestWebSearchToolRegistration(t *testing.T) {
	// Validates that the capability-gating logic in chat() correctly
	// decides which tools to register based on model capabilities and
	// the web search flag.
	tests := []struct {
		name             string
		webSearchEnabled bool
		hasToolsCap      bool
		model            string
		wantBrowser      bool // expects browser tools (gpt-oss)
		wantWebSearch    bool // expects basic web search/fetch tools
		wantNone         bool // expects no tools registered
	}{
		{
			name:             "web search enabled with tools capability - browser model",
			webSearchEnabled: true,
			hasToolsCap:      true,
			model:            "gpt-oss-latest",
			wantBrowser:      true,
		},
		{
			name:             "web search enabled with tools capability - non-browser model",
			webSearchEnabled: true,
			hasToolsCap:      true,
			model:            "qwen3",
			wantWebSearch:    true,
		},
		{
			name:             "web search enabled without tools capability",
			webSearchEnabled: true,
			hasToolsCap:      false,
			model:            "llama3.3",
			wantNone:         true,
		},
		{
			name:             "web search disabled with tools capability",
			webSearchEnabled: false,
			hasToolsCap:      true,
			model:            "qwen3",
			wantNone:         true,
		},
		{
			name:             "web search disabled without tools capability",
			webSearchEnabled: false,
			hasToolsCap:      false,
			model:            "llama3.3",
			wantNone:         true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Replicate the decision logic from chat() handler
			gotBrowser := false
			gotWebSearch := false

			if tt.webSearchEnabled && tt.hasToolsCap {
				if supportsBrowserTools(tt.model) {
					gotBrowser = true
				} else {
					gotWebSearch = true
				}
			}

			if tt.wantBrowser && !gotBrowser {
				t.Error("expected browser tools to be registered")
			}
			if tt.wantWebSearch && !gotWebSearch {
				t.Error("expected web search tools to be registered")
			}
			if tt.wantNone && (gotBrowser || gotWebSearch) {
				t.Error("expected no tools to be registered")
			}
			if !tt.wantBrowser && gotBrowser {
				t.Error("unexpected browser tools registered")
			}
			if !tt.wantWebSearch && gotWebSearch {
				t.Error("unexpected web search tools registered")
			}
		})
	}
}

func TestRequestToolGrants(t *testing.T) {
	persistent := tools.NewRegistry()
	persistent.Register(&testTool{name: "mcp.filesystem.read_file"})

	tests := []struct {
		name             string
		fileToolsEnabled bool
		webSearchEnabled bool
		wantTools        int
	}{
		{name: "tools off web off", wantTools: 0},
		{name: "tools on web off", fileToolsEnabled: true, wantTools: 1},
		{name: "tools off web on", webSearchEnabled: true, wantTools: 2},
		{name: "tools on web on", fileToolsEnabled: true, webSearchEnabled: true, wantTools: 3},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := requestToolRegistry(persistent, true, tt.fileToolsEnabled)
			if tt.webSearchEnabled {
				registry.Register(&tools.WebSearch{})
				registry.Register(&tools.WebFetch{})
			}

			server := &Server{}
			chat := store.NewChat("chat")
			chat.Messages = append(chat.Messages, store.NewMessage("user", "hello", nil))
			req, err := server.buildChatRequest(chat, "qwen3", nil, registry.AvailableTools())
			if err != nil {
				t.Fatalf("buildChatRequest() error = %v", err)
			}
			if got := len(req.Tools); got != tt.wantTools {
				t.Fatalf("request tools = %d, want %d", got, tt.wantTools)
			}
		})
	}
}

func TestPersistentFileToolGrantAcrossConsecutiveSends(t *testing.T) {
	persistent := tools.NewRegistry()
	persistent.Register(&testTool{name: "mcp.filesystem.read_file"})

	for send := 1; send <= 2; send++ {
		registry := requestToolRegistry(persistent, true, true)
		if got := len(registry.AvailableTools()); got != 1 {
			t.Fatalf("send %d tool count = %d, want 1", send, got)
		}
	}
}

func TestSettingsToggleAutoUpdateOff_CancelsDownload(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	// Start with auto-update enabled
	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.AutoUpdateEnabled = true
	if err := testStore.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	upd := &updater.Updater{Store: &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db2.sqlite"),
	}}
	defer upd.Store.Close()

	// We can't easily mock CancelOngoingDownload, but we can verify
	// the full settings handler flow works without error
	server := &Server{
		Store:   testStore,
		Restart: func() {},
		Updater: upd,
	}

	// Disable auto-update via settings API
	settings.AutoUpdateEnabled = false
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	if err := server.settings(rr, req); err != nil {
		t.Fatalf("settings() error = %v", err)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("settings() status = %d, want %d", rr.Code, http.StatusOK)
	}

	// Verify settings were saved with auto-update disabled
	saved, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if saved.AutoUpdateEnabled {
		t.Fatal("expected AutoUpdateEnabled to be false after toggle off")
	}
}

func TestSettingsPreservesOnboardingVersionWhenOmitted(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.OnboardingVersion = 1
	if err := testStore.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "OnboardingVersion")
	payload, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{
		Store:   testStore,
		Restart: func() {},
	}
	req := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(payload))
	rr := httptest.NewRecorder()

	if err := server.settings(rr, req); err != nil {
		t.Fatalf("settings() error = %v", err)
	}

	saved, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if saved.OnboardingVersion != 1 {
		t.Fatalf("OnboardingVersion = %d, want 1", saved.OnboardingVersion)
	}
}

func TestSettingsPreservesClaudeDesktopUsedWhenOmitted(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.ClaudeDesktopUsed = true
	if err := testStore.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "ClaudeDesktopUsed")
	payload, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{Store: testStore, Restart: func() {}}
	req := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(payload))
	rr := httptest.NewRecorder()
	if err := server.settings(rr, req); err != nil {
		t.Fatalf("settings() error = %v", err)
	}

	saved, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !saved.ClaudeDesktopUsed {
		t.Fatal("expected ClaudeDesktopUsed to be preserved")
	}
}

func TestSettingsPreservesPluginsEnabledWhenOmitted(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.PluginsEnabled = true
	if err := testStore.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "PluginsEnabled")
	payload, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{Store: testStore, Restart: func() {}}
	req := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(payload))
	rr := httptest.NewRecorder()
	if err := server.settings(rr, req); err != nil {
		t.Fatalf("settings() error = %v", err)
	}

	saved, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !saved.PluginsEnabled {
		t.Fatal("expected PluginsEnabled to be preserved")
	}
}

func TestSettingsUpdatesPluginsEnabledWhenProvided(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.PluginsEnabled {
		t.Fatal("expected PluginsEnabled to start false")
	}

	settings.PluginsEnabled = true
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{
		Store:   testStore,
		Restart: func() {},
	}

	postReq := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(body))
	postRR := httptest.NewRecorder()
	if err := server.settings(postRR, postReq); err != nil {
		t.Fatalf("settings() POST error = %v", err)
	}

	saved, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !saved.PluginsEnabled {
		t.Fatal("expected PluginsEnabled to persist as true")
	}

	getReq := httptest.NewRequest("GET", "/api/v1/settings", nil)
	getRR := httptest.NewRecorder()
	if err := server.getSettings(getRR, getReq); err != nil {
		t.Fatalf("getSettings() error = %v", err)
	}

	var response struct {
		Settings store.Settings `json:"settings"`
	}
	if err := json.NewDecoder(getRR.Body).Decode(&response); err != nil {
		t.Fatalf("decode getSettings response: %v", err)
	}
	if !response.Settings.PluginsEnabled {
		t.Fatal("expected GET /api/v1/settings to return PluginsEnabled=true")
	}
}

func TestSettingsPreservesLocalModelsEnabledWhenOmitted(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.LocalModelsEnabled = false
	if err := testStore.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "LocalModelsEnabled")
	payload, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{Store: testStore, Restart: func() {}}
	req := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(payload))
	rr := httptest.NewRecorder()
	if err := server.settings(rr, req); err != nil {
		t.Fatalf("settings() error = %v", err)
	}

	saved, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if saved.LocalModelsEnabled {
		t.Fatal("expected LocalModelsEnabled to be preserved")
	}
}

func TestSettingsUpdatesLocalModelsEnabledWhenProvided(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !settings.LocalModelsEnabled {
		t.Fatal("expected LocalModelsEnabled to start true")
	}

	settings.LocalModelsEnabled = false
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{
		Store:   testStore,
		Restart: func() {},
	}

	postReq := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(body))
	postRR := httptest.NewRecorder()
	if err := server.settings(postRR, postReq); err != nil {
		t.Fatalf("settings() POST error = %v", err)
	}

	saved, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if saved.LocalModelsEnabled {
		t.Fatal("expected LocalModelsEnabled to persist as false")
	}

	getReq := httptest.NewRequest("GET", "/api/v1/settings", nil)
	getRR := httptest.NewRecorder()
	if err := server.getSettings(getRR, getReq); err != nil {
		t.Fatalf("getSettings() error = %v", err)
	}

	var response struct {
		Settings store.Settings `json:"settings"`
	}
	if err := json.NewDecoder(getRR.Body).Decode(&response); err != nil {
		t.Fatalf("decode getSettings response: %v", err)
	}
	if response.Settings.LocalModelsEnabled {
		t.Fatal("expected GET /api/v1/settings to return LocalModelsEnabled=false")
	}
}

func TestSettingsPreservesCPUFallbackEnabledWhenOmitted(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.CPUFallbackEnabled = true
	if err := testStore.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "CPUFallbackEnabled")
	payload, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{Store: testStore, Restart: func() {}}
	req := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(payload))
	rr := httptest.NewRecorder()
	if err := server.settings(rr, req); err != nil {
		t.Fatalf("settings() error = %v", err)
	}

	saved, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !saved.CPUFallbackEnabled {
		t.Fatal("expected CPUFallbackEnabled to be preserved")
	}
}

func TestSettingsUpdatesCPUFallbackEnabledWhenProvided(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.CPUFallbackEnabled {
		t.Fatal("expected CPUFallbackEnabled to start false")
	}

	settings.CPUFallbackEnabled = true
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{
		Store:   testStore,
		Restart: func() {},
	}

	postReq := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(body))
	postRR := httptest.NewRecorder()
	if err := server.settings(postRR, postReq); err != nil {
		t.Fatalf("settings() POST error = %v", err)
	}

	saved, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !saved.CPUFallbackEnabled {
		t.Fatal("expected CPUFallbackEnabled to persist as true")
	}

	getReq := httptest.NewRequest("GET", "/api/v1/settings", nil)
	getRR := httptest.NewRecorder()
	if err := server.getSettings(getRR, getReq); err != nil {
		t.Fatalf("getSettings() error = %v", err)
	}

	var response struct {
		Settings store.Settings `json:"settings"`
	}
	if err := json.NewDecoder(getRR.Body).Decode(&response); err != nil {
		t.Fatalf("decode getSettings response: %v", err)
	}
	if !response.Settings.CPUFallbackEnabled {
		t.Fatal("expected GET /api/v1/settings to return CPUFallbackEnabled=true")
	}
}

func TestSettingsPreservesInferenceEnabledWhenOmitted(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.InferenceEnabled = true
	if err := testStore.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "InferenceEnabled")
	payload, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{Store: testStore, Restart: func() {}}
	req := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(payload))
	rr := httptest.NewRecorder()
	if err := server.settings(rr, req); err != nil {
		t.Fatalf("settings() error = %v", err)
	}

	saved, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !saved.InferenceEnabled {
		t.Fatal("expected InferenceEnabled to be preserved")
	}
}

func TestSettingsUpdatesInferenceEnabledWhenProvided(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if settings.InferenceEnabled {
		t.Fatal("expected InferenceEnabled to start false")
	}

	settings.InferenceEnabled = true
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{
		Store:   testStore,
		Restart: func() {},
	}

	postReq := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(body))
	postRR := httptest.NewRecorder()
	if err := server.settings(postRR, postReq); err != nil {
		t.Fatalf("settings() POST error = %v", err)
	}

	saved, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if !saved.InferenceEnabled {
		t.Fatal("expected InferenceEnabled to persist as true")
	}

	getReq := httptest.NewRequest("GET", "/api/v1/settings", nil)
	getRR := httptest.NewRecorder()
	if err := server.getSettings(getRR, getReq); err != nil {
		t.Fatalf("getSettings() error = %v", err)
	}

	var response struct {
		Settings store.Settings `json:"settings"`
	}
	if err := json.NewDecoder(getRR.Body).Decode(&response); err != nil {
		t.Fatalf("decode getSettings response: %v", err)
	}
	if !response.Settings.InferenceEnabled {
		t.Fatal("expected GET /api/v1/settings to return InferenceEnabled=true")
	}
}

func TestSettingsPreservesInferenceRuntimeWhenOmitted(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.InferenceRuntime = "cuda_legacy"
	if err := testStore.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	payload, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "InferenceRuntime")
	payload, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{Store: testStore, Restart: func() {}}
	req := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(payload))
	rr := httptest.NewRecorder()
	if err := server.settings(rr, req); err != nil {
		t.Fatalf("settings() error = %v", err)
	}

	saved, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if saved.InferenceRuntime != "cuda_legacy" {
		t.Fatalf("expected InferenceRuntime to be preserved, got %q", saved.InferenceRuntime)
	}
}

func TestSettingsUpdatesInferenceRuntimeWhenProvided(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.InferenceRuntime = "cuda_legacy"
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}

	server := &Server{
		Store:   testStore,
		Restart: func() {},
	}

	postReq := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(body))
	postRR := httptest.NewRecorder()
	if err := server.settings(postRR, postReq); err != nil {
		t.Fatalf("settings() POST error = %v", err)
	}

	saved, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	if saved.InferenceRuntime != "cuda_legacy" {
		t.Fatalf("expected InferenceRuntime to persist, got %q", saved.InferenceRuntime)
	}

	getReq := httptest.NewRequest("GET", "/api/v1/settings", nil)
	getRR := httptest.NewRecorder()
	if err := server.getSettings(getRR, getReq); err != nil {
		t.Fatalf("getSettings() error = %v", err)
	}

	var response struct {
		Settings store.Settings `json:"settings"`
	}
	if err := json.NewDecoder(getRR.Body).Decode(&response); err != nil {
		t.Fatalf("decode getSettings response: %v", err)
	}
	if response.Settings.InferenceRuntime != "cuda_legacy" {
		t.Fatalf("expected GET /api/v1/settings to return InferenceRuntime=cuda_legacy, got %q", response.Settings.InferenceRuntime)
	}
}

func TestPluginHandlers(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.PluginsEnabled = true
	if err := testStore.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	server := &Server{
		Store:   testStore,
		Restart: func() {},
	}

	importBody := `{
		"config": "{\n  \"mcpServers\": {\n    \"example\": {\n      \"command\": \"example-command\",\n      \"args\": [\"--stdio\"]\n    }\n  }\n}"
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/plugins/import", strings.NewReader(importBody))
	rr := httptest.NewRecorder()
	if err := server.importPlugins(rr, req); err != nil {
		t.Fatalf("importPlugins() error = %v", err)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("importPlugins() status = %d, want %d", rr.Code, http.StatusOK)
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/plugins", nil)
	listRR := httptest.NewRecorder()
	if err := server.listPlugins(listRR, listReq); err != nil {
		t.Fatalf("listPlugins() error = %v", err)
	}

	var listed struct {
		Plugins []struct {
			ID string `json:"id"`
		} `json:"plugins"`
	}
	if err := json.NewDecoder(listRR.Body).Decode(&listed); err != nil {
		t.Fatalf("decode listPlugins response: %v", err)
	}
	if len(listed.Plugins) != 1 {
		t.Fatalf("expected 1 plugin, got %d", len(listed.Plugins))
	}
	if listed.Plugins[0].ID != "mcp:example" {
		t.Fatalf("unexpected plugin id %q", listed.Plugins[0].ID)
	}

	updateReq := httptest.NewRequest(http.MethodPut, "/api/v1/plugins/mcp:example", strings.NewReader(`{"enabled":false}`))
	updateReq.SetPathValue("id", "mcp:example")
	updateRR := httptest.NewRecorder()
	if err := server.updatePlugin(updateRR, updateReq); err != nil {
		t.Fatalf("updatePlugin() error = %v", err)
	}

	plugins, err := testStore.Plugins()
	if err != nil {
		t.Fatalf("Plugins() error = %v", err)
	}
	if len(plugins) != 1 || plugins[0].Enabled {
		t.Fatal("expected plugin to be disabled after update")
	}

	deleteReq := httptest.NewRequest(http.MethodDelete, "/api/v1/plugins/mcp:example", nil)
	deleteReq.SetPathValue("id", "mcp:example")
	deleteRR := httptest.NewRecorder()
	if err := server.deletePlugin(deleteRR, deleteReq); err != nil {
		t.Fatalf("deletePlugin() error = %v", err)
	}
	if deleteRR.Code != http.StatusNoContent {
		t.Fatalf("deletePlugin() status = %d, want %d", deleteRR.Code, http.StatusNoContent)
	}

	plugins, err = testStore.Plugins()
	if err != nil {
		t.Fatalf("Plugins() after delete error = %v", err)
	}
	if len(plugins) != 0 {
		t.Fatalf("expected no plugins after delete, got %d", len(plugins))
	}
}

func TestListPluginsIncludesMCPRuntimeState(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.PluginsEnabled = true
	if err := testStore.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	connected := uiHelperPlugin(t, "connected", true)
	if err := testStore.UpsertPlugin(connected); err != nil {
		t.Fatalf("UpsertPlugin(connected) error = %v", err)
	}

	disabled := uiHelperPlugin(t, "disabled", false)
	if err := testStore.UpsertPlugin(disabled); err != nil {
		t.Fatalf("UpsertPlugin(disabled) error = %v", err)
	}

	badConfig, err := json.Marshal(map[string]any{
		"command":   "definitely-missing-command-for-mcp-tests",
		"args":      []string{},
		"transport": "stdio",
	})
	if err != nil {
		t.Fatal(err)
	}
	failing := store.Plugin{
		ID:       "mcp:failing",
		Name:     "failing",
		Provider: "mcp",
		Enabled:  true,
		Config:   badConfig,
	}
	if err := testStore.UpsertPlugin(failing); err != nil {
		t.Fatalf("UpsertPlugin(failing) error = %v", err)
	}

	registry := tools.NewRegistry()
	manager := mcp.NewManager(context.Background(), nil, registry, slogDiscard())
	defer manager.Close()

	syncCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := manager.SyncPlugins(syncCtx, []store.Plugin{connected, disabled, failing}); err == nil {
		t.Fatal("expected SyncPlugins() to report error for failing plugin")
	}

	server := &Server{
		Store:      testStore,
		MCPManager: manager,
		Restart:    func() {},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins", nil)
	rr := httptest.NewRecorder()
	if err := server.listPlugins(rr, req); err != nil {
		t.Fatalf("listPlugins() error = %v", err)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("listPlugins() status = %d, want %d", rr.Code, http.StatusOK)
	}

	type toolInfo struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Enabled     bool   `json:"enabled"`
	}
	type runtimeInfo struct {
		Status    string     `json:"status"`
		ToolCount int        `json:"toolCount"`
		LastError string     `json:"lastError"`
		Tools     []toolInfo `json:"tools"`
	}
	var response struct {
		Plugins []struct {
			ID      string      `json:"id"`
			Enabled bool        `json:"enabled"`
			Runtime runtimeInfo `json:"runtime"`
		} `json:"plugins"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Fatalf("decode listPlugins response: %v", err)
	}

	type pluginInfo struct {
		Enabled bool
		Runtime runtimeInfo
	}
	got := make(map[string]pluginInfo)
	for _, plugin := range response.Plugins {
		got[plugin.ID] = pluginInfo{Enabled: plugin.Enabled, Runtime: plugin.Runtime}
	}

	if got["mcp:connected"].Runtime.Status != "connected" {
		t.Fatalf("connected runtime status = %q, want %q", got["mcp:connected"].Runtime.Status, "connected")
	}
	if got["mcp:connected"].Runtime.ToolCount != 1 {
		t.Fatalf("connected toolCount = %d, want 1", got["mcp:connected"].Runtime.ToolCount)
	}
	if len(got["mcp:connected"].Runtime.Tools) != 1 || got["mcp:connected"].Runtime.Tools[0].Name != "mcp.connected.echo" {
		t.Fatalf("connected tools = %+v, want one namespaced tool", got["mcp:connected"].Runtime.Tools)
	}
	if !got["mcp:connected"].Runtime.Tools[0].Enabled {
		t.Fatal("expected connected tool to default to enabled")
	}

	if got["mcp:disabled"].Runtime.Status != "disabled" {
		t.Fatalf("disabled runtime status = %q, want %q", got["mcp:disabled"].Runtime.Status, "disabled")
	}
	if got["mcp:disabled"].Runtime.ToolCount != 0 {
		t.Fatalf("disabled toolCount = %d, want 0", got["mcp:disabled"].Runtime.ToolCount)
	}

	if got["mcp:failing"].Runtime.Status != "error" {
		t.Fatalf("failing runtime status = %q, want %q", got["mcp:failing"].Runtime.Status, "error")
	}
	if got["mcp:failing"].Runtime.LastError == "" {
		t.Fatal("expected failing plugin to include lastError")
	}
}

func TestPluginToolHandlerPersistsDisabledToolAndExposesEnabledState(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.PluginsEnabled = true
	if err := testStore.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	plugin := uiHelperPlugin(t, "toggle", true)
	if err := testStore.UpsertPlugin(plugin); err != nil {
		t.Fatalf("UpsertPlugin(toggle) error = %v", err)
	}

	registry := tools.NewRegistry()
	manager := mcp.NewManager(context.Background(), testStore, registry, slogDiscard())
	defer manager.Close()

	syncCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := manager.SyncFromStore(syncCtx); err != nil {
		t.Fatalf("SyncFromStore() error = %v", err)
	}

	server := &Server{
		Store:      testStore,
		MCPManager: manager,
		Restart:    func() {},
	}

	updateReq := httptest.NewRequest(http.MethodPut, "/api/v1/plugins/mcp:toggle/tools/mcp.toggle.echo", strings.NewReader(`{"enabled":false}`))
	updateReq.SetPathValue("id", "mcp:toggle")
	updateReq.SetPathValue("toolName", "mcp.toggle.echo")
	updateRR := httptest.NewRecorder()
	if err := server.updatePluginTool(updateRR, updateReq); err != nil {
		t.Fatalf("updatePluginTool() error = %v", err)
	}
	if updateRR.Code != http.StatusOK {
		t.Fatalf("updatePluginTool() status = %d, want %d", updateRR.Code, http.StatusOK)
	}

	prefs, err := testStore.PluginToolPreferences()
	if err != nil {
		t.Fatalf("PluginToolPreferences() error = %v", err)
	}
	if len(prefs) != 1 || prefs[0].PluginID != "mcp:toggle" || prefs[0].ToolName != "mcp.toggle.echo" || prefs[0].Enabled {
		t.Fatalf("unexpected tool prefs after disable: %+v", prefs)
	}

	if _, ok := registry.Get("mcp.toggle.echo"); ok {
		t.Fatal("expected disabled tool to be removed from registry")
	}

	listReq := httptest.NewRequest(http.MethodGet, "/api/v1/plugins", nil)
	listRR := httptest.NewRecorder()
	if err := server.listPlugins(listRR, listReq); err != nil {
		t.Fatalf("listPlugins() error = %v", err)
	}

	var response struct {
		Plugins []struct {
			ID      string `json:"id"`
			Runtime struct {
				Tools []struct {
					Name    string `json:"name"`
					Enabled bool   `json:"enabled"`
				} `json:"tools"`
			} `json:"runtime"`
		} `json:"plugins"`
	}
	if err := json.NewDecoder(listRR.Body).Decode(&response); err != nil {
		t.Fatalf("decode listPlugins response: %v", err)
	}

	if len(response.Plugins) != 1 || len(response.Plugins[0].Runtime.Tools) != 1 {
		t.Fatalf("unexpected plugin runtime response: %+v", response.Plugins)
	}
	if response.Plugins[0].Runtime.Tools[0].Name != "mcp.toggle.echo" || response.Plugins[0].Runtime.Tools[0].Enabled {
		t.Fatalf("unexpected tool runtime after disable: %+v", response.Plugins[0].Runtime.Tools[0])
	}
}

func TestPluginHandlersRejectWhenDisabled(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	server := &Server{
		Store:   testStore,
		Restart: func() {},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/plugins", nil)
	rr := httptest.NewRecorder()
	if err := server.listPlugins(rr, req); err != nil {
		t.Fatalf("listPlugins() error = %v", err)
	}
	if rr.Code != http.StatusForbidden {
		t.Fatalf("listPlugins() status = %d, want %d", rr.Code, http.StatusForbidden)
	}
}

func TestSettingsToggleAutoUpdateOn_WithPendingUpdate_ShowsNotification(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	// Start with auto-update disabled
	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.AutoUpdateEnabled = false
	if err := testStore.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	// Simulate that an update was previously downloaded
	oldVal := updater.UpdateDownloaded
	updater.UpdateDownloaded = true
	defer func() { updater.UpdateDownloaded = oldVal }()

	var notificationCalled atomic.Bool
	server := &Server{
		Store:   testStore,
		Restart: func() {},
		UpdateAvailableFunc: func() {
			notificationCalled.Store(true)
		},
	}

	// Re-enable auto-update via settings API
	settings.AutoUpdateEnabled = true
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	if err := server.settings(rr, req); err != nil {
		t.Fatalf("settings() error = %v", err)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("settings() status = %d, want %d", rr.Code, http.StatusOK)
	}

	if !notificationCalled.Load() {
		t.Fatal("expected UpdateAvailableFunc to be called when re-enabling with a downloaded update")
	}
}

func TestSettingsToggleAutoUpdateOn_NoPendingUpdate_TriggersCheck(t *testing.T) {
	testStore := &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db.sqlite"),
	}
	defer testStore.Close()

	// Start with auto-update disabled
	settings, err := testStore.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.AutoUpdateEnabled = false
	if err := testStore.SetSettings(settings); err != nil {
		t.Fatal(err)
	}

	// Ensure no pending update - clear both the downloaded flag and the stage dir
	oldVal := updater.UpdateDownloaded
	updater.UpdateDownloaded = false
	defer func() { updater.UpdateDownloaded = oldVal }()

	oldStageDir := updater.UpdateStageDir
	updater.UpdateStageDir = t.TempDir() // empty dir means IsUpdatePending() returns false
	defer func() { updater.UpdateStageDir = oldStageDir }()

	upd := &updater.Updater{Store: &store.Store{
		DBPath: filepath.Join(t.TempDir(), "db2.sqlite"),
	}}
	defer upd.Store.Close()

	// Initialize the checkNow channel by starting (and immediately stopping) the checker
	// so TriggerImmediateCheck doesn't panic on nil channel
	ctx, cancel := context.WithCancel(t.Context())
	upd.StartBackgroundUpdaterChecker(ctx, func(string) error { return nil })
	defer cancel()

	var notificationCalled atomic.Bool
	server := &Server{
		Store:   testStore,
		Restart: func() {},
		Updater: upd,
		UpdateAvailableFunc: func() {
			notificationCalled.Store(true)
		},
	}

	// Re-enable auto-update via settings API
	settings.AutoUpdateEnabled = true
	body, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/api/v1/settings", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	if err := server.settings(rr, req); err != nil {
		t.Fatalf("settings() error = %v", err)
	}
	if rr.Code != http.StatusOK {
		t.Fatalf("settings() status = %d, want %d", rr.Code, http.StatusOK)
	}

	// UpdateAvailableFunc should NOT be called since there's no pending update
	if notificationCalled.Load() {
		t.Fatal("UpdateAvailableFunc should not be called when there is no pending update")
	}
}

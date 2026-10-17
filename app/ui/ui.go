//go:build windows || darwin

// package ui implements a chat interface for Ollama
package ui

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/app/mcp"
	"github.com/ollama/ollama/app/server"
	"github.com/ollama/ollama/app/store"
	"github.com/ollama/ollama/app/tools"
	"github.com/ollama/ollama/app/types/not"
	"github.com/ollama/ollama/app/ui/responses"
	"github.com/ollama/ollama/app/updater"
	"github.com/ollama/ollama/app/version"
	ollamaAuth "github.com/ollama/ollama/auth"
	"github.com/ollama/ollama/cmd/launch"
	"github.com/ollama/ollama/envconfig"
	"github.com/ollama/ollama/format"
	"github.com/ollama/ollama/fs/ggml"
	"github.com/ollama/ollama/manifest"
	"github.com/ollama/ollama/types/model"
	_ "github.com/tkrajina/typescriptify-golang-structs/typescriptify"
)

//go:generate tscriptify -package=github.com/ollama/ollama/app/ui/responses -target=./app/codegen/gotypes.gen.ts responses/types.go
//go:generate npm --prefix ./app run build

var CORS = envconfig.Bool("OLLAMA_CORS")

// OllamaDotCom returns the URL for ollama.com, allowing override via environment variable
var OllamaDotCom = func() string {
	if url := os.Getenv("OLLAMA_DOT_COM_URL"); url != "" {
		return url
	}
	return "https://ollama.com"
}()

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) Written() bool {
	return r.code != 0
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Status() int {
	if r.code == 0 {
		return http.StatusOK
	}
	return r.code
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// Event is a string that represents the type of event being sent to the
// client. It is used in the Server-Sent Events (SSE) protocol to identify
// the type of data being sent.
// The client (template) will use this type in the sse event listener to
// determine how to handle the incoming data. It will also be used in the
// sse-swap htmx event listener to determine how to handle the incoming data.
type Event string

const (
	EventChat       Event = "chat"
	EventComplete   Event = "complete"
	EventLoading    Event = "loading"
	EventToolResult Event = "tool_result" // Used for both tool calls and their results
	EventThinking   Event = "thinking"
	EventToolCall   Event = "tool_call"
	EventDownload   Event = "download"
)

type Server struct {
	Logger       *slog.Logger
	Restart      func()
	Token        string
	Store        *store.Store
	MCPManager   *mcp.Manager
	ToolRegistry *tools.Registry
	Tools        bool   // if true, the server will use single-turn tools to fulfill the user's request
	WebSearch    bool   // if true, the server will use single-turn browser tool to fulfill the user's request
	Agent        bool   // if true, the server will use multi-turn tools to fulfill the user's request
	WorkingDir   string // Working directory for all agent operations

	// Dev is true if the server is running in development mode
	Dev bool

	// Updater for checking and downloading updates
	Updater              *updater.Updater
	UpdateAvailableFunc  func()
	IntegrationInstalled func(string) bool
	ListCloudModels      func(context.Context) (*api.ListResponse, error)
}

func (s *Server) log() *slog.Logger {
	if s.Logger == nil {
		return slog.Default()
	}
	return s.Logger
}

// ollamaProxy creates a reverse proxy handler to the Ollama server
func (s *Server) ollamaProxy() http.Handler {
	var (
		proxy   http.Handler
		proxyMu sync.Mutex
	)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyMu.Lock()
		p := proxy
		proxyMu.Unlock()

		if p == nil {
			proxyMu.Lock()
			if proxy == nil {
				var err error
				for i := range 2 {
					if i > 0 {
						s.log().Warn("ollama server not ready, retrying", "attempt", i+1)
						time.Sleep(1 * time.Second)
					}

					err = WaitForServer(context.Background(), 10*time.Second)
					if err == nil {
						break
					}
				}

				if err != nil {
					proxyMu.Unlock()
					s.log().Error("ollama server not ready after retries", "error", err)
					http.Error(w, "Ollama server is not ready", http.StatusServiceUnavailable)
					return
				}

				target := envconfig.ConnectableHost()
				s.log().Info("configuring ollama proxy", "target", target.String())

				newProxy := httputil.NewSingleHostReverseProxy(target)

				originalDirector := newProxy.Director
				newProxy.Director = func(req *http.Request) {
					originalDirector(req)
					req.Host = target.Host
					s.log().Debug("proxying request", "method", req.Method, "path", req.URL.Path, "target", target.Host)
				}

				newProxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
					s.log().Error("proxy error", "error", err, "path", r.URL.Path, "target", target.String())
					http.Error(w, "proxy error: "+err.Error(), http.StatusBadGateway)
				}

				proxy = newProxy
				p = newProxy
			} else {
				p = proxy
			}
			proxyMu.Unlock()
		}

		p.ServeHTTP(w, r)
	})
}

type errHandlerFunc func(http.ResponseWriter, *http.Request) error

func (s *Server) Handler() http.Handler {
	handle := func(f errHandlerFunc) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Add CORS headers for dev work
			if CORS() {
				w.Header().Set("Access-Control-Allow-Origin", "*")
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, User-Agent, Accept, X-Requested-With")
				w.Header().Set("Access-Control-Allow-Credentials", "true")

				// Handle preflight requests
				if r.Method == "OPTIONS" {
					w.WriteHeader(http.StatusOK)
					return
				}
			}

			// Don't check for token in development mode
			if !s.Dev {
				cookie, err := r.Cookie("token")
				if err != nil {
					w.WriteHeader(http.StatusForbidden)
					json.NewEncoder(w).Encode(map[string]string{"error": "Token is required"})
					return
				}

				if cookie.Value != s.Token {
					w.WriteHeader(http.StatusForbidden)
					json.NewEncoder(w).Encode(map[string]string{"error": "Token is required"})
					return
				}
			}

			sw := &statusRecorder{ResponseWriter: w}

			log := s.log()
			level := slog.LevelInfo
			start := time.Now()
			requestID := fmt.Sprintf("%d", time.Now().UnixNano())

			defer func() {
				p := recover()
				if p != nil {
					log = log.With("panic", p, "request_id", requestID)
					level = slog.LevelError

					// Handle panic with user-friendly error
					if !sw.Written() {
						s.handleError(sw, fmt.Errorf("internal server error"))
					}
				}

				log.Log(r.Context(), level, "site.serveHTTP",
					"http.method", r.Method,
					"http.path", r.URL.Path,
					"http.pattern", r.Pattern,
					"http.status", sw.Status(),
					"http.d", time.Since(start),
					"request_id", requestID,
					"version", version.Version,
				)

				// let net/http.Server deal with panics
				if p != nil {
					panic(p)
				}
			}()

			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("X-Version", version.Version)
			w.Header().Set("X-Request-ID", requestID)

			ctx := r.Context()
			if err := f(sw, r); err != nil {
				if ctx.Err() != nil {
					return
				}
				level = slog.LevelError
				log = log.With("error", err)
				s.handleError(sw, err)
			}
		})
	}

	mux := http.NewServeMux()

	// CORS is handled in `handle`, but we have to match on OPTIONS to handle preflight requests
	mux.Handle("OPTIONS /", handle(func(w http.ResponseWriter, r *http.Request) error {
		return nil
	}))

	// API routes - handle first to take precedence
	mux.Handle("GET /api/v1/chats", handle(s.listChats))
	mux.Handle("GET /api/v1/chat/{id}", handle(s.getChat))
	mux.Handle("POST /api/v1/chat/{id}", handle(s.chat))
	mux.Handle("DELETE /api/v1/chat/{id}", handle(s.deleteChat))
	mux.Handle("POST /api/v1/create-chat", handle(s.createChat))
	mux.Handle("PUT /api/v1/chat/{id}/rename", handle(s.renameChat))

	mux.Handle("GET /api/v1/inference-compute", handle(s.getInferenceCompute))
	mux.Handle("POST /api/v1/model/upstream", handle(s.modelUpstream))
	mux.Handle("GET /api/v1/models/manage", handle(s.listManagedModels))
	mux.Handle("POST /api/v1/models/link", handle(s.linkModelFiles))
	mux.Handle("POST /api/v1/models/import", handle(s.importModelFiles))
	mux.Handle("POST /api/v1/models/alias", handle(s.setModelAlias))
	mux.Handle("POST /api/v1/models/unlink", handle(s.unlinkModel))
	mux.Handle("POST /api/v1/models/delete", handle(s.deleteManagedModel))
	mux.Handle("GET /api/v1/settings", handle(s.getSettings))
	mux.Handle("POST /api/v1/settings", handle(s.settings))
	mux.Handle("GET /api/v1/cloud", handle(s.getCloudSetting))
	mux.Handle("POST /api/v1/cloud", handle(s.cloudSetting))
	mux.Handle("GET /api/v1/models/cloud", handle(s.getCloudModels))
	mux.Handle("GET /api/v1/integrations", handle(s.getIntegrationStatuses))
	mux.Handle("GET /api/v1/plugins", handle(s.listPlugins))
	mux.Handle("POST /api/v1/plugins/import", handle(s.importPlugins))
	mux.Handle("PUT /api/v1/plugins/{id}", handle(s.updatePlugin))
	mux.Handle("PUT /api/v1/plugins/{id}/tools/{toolName}", handle(s.updatePluginTool))
	mux.Handle("DELETE /api/v1/plugins/{id}", handle(s.deletePlugin))

	// Ollama proxy endpoints
	ollamaProxy := s.ollamaProxy()
	mux.Handle("GET /api/tags", ollamaProxy)
	mux.Handle("GET /api/ps", ollamaProxy)
	mux.Handle("POST /api/show", ollamaProxy)
	mux.Handle("GET /api/version", ollamaProxy)
	mux.Handle("GET /api/status", ollamaProxy)
	mux.Handle("HEAD /api/version", ollamaProxy)
	mux.Handle("POST /api/me", ollamaProxy)
	mux.Handle("POST /api/signout", ollamaProxy)
	mux.Handle("GET /api/experimental/model-recommendations", ollamaProxy)

	// React app - catch all non-API routes and serve the React app
	mux.Handle("GET /", s.appHandler())
	mux.Handle("PUT /", s.appHandler())
	mux.Handle("POST /", s.appHandler())
	mux.Handle("PATCH /", s.appHandler())
	mux.Handle("DELETE /", s.appHandler())

	return mux
}

func (s *Server) getIntegrationStatuses(w http.ResponseWriter, _ *http.Request) error {
	isInstalled := s.IntegrationInstalled
	if isInstalled == nil {
		isInstalled = launch.IsIntegrationInstalled
	}

	type integrationStatus struct {
		ID          string `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
		Installed   *bool  `json:"installed,omitempty"`
		Action      string `json:"action"`
		Command     string `json:"command,omitempty"`
	}

	infos := launch.ListIntegrationInfos()
	statuses := make([]integrationStatus, 0, len(infos)+2)
	claudeDesktopInstalled := isInstalled("claude-desktop")
	statuses = append(statuses, integrationStatus{
		ID:          "claude-desktop",
		Name:        "Claude",
		Description: "Use Ollama models in Claude Desktop",
		Installed:   &claudeDesktopInstalled,
		Action:      "connect",
	})

	byName := make(map[string]launch.IntegrationInfo, len(infos))
	for _, info := range infos {
		byName[info.Name] = info
	}
	seen := map[string]bool{"chatgpt": true}
	launcherMenuOrder := []string{"claude", "codex", "openclaw", "opencode", "droid", "pi", "cline"}
	orderedInfos := make([]launch.IntegrationInfo, 0, len(infos))
	for _, name := range launcherMenuOrder {
		if info, ok := byName[name]; ok {
			orderedInfos = append(orderedInfos, info)
			seen[name] = true
		}
	}
	for _, info := range infos {
		if !seen[info.Name] {
			orderedInfos = append(orderedInfos, info)
		}
	}

	for _, info := range orderedInfos {
		installed := isInstalled(info.Name)
		statuses = append(statuses, integrationStatus{
			ID:          info.Name,
			Name:        info.DisplayName,
			Description: info.Description,
			Installed:   &installed,
			Action:      "copy",
			Command:     "ollama launch " + info.Name,
		})
	}

	statuses = append(statuses, integrationStatus{
		ID:          "terminal",
		Name:        "Terminal",
		Description: "Run local models from your terminal",
		Action:      "copy",
		Command:     "ollama",
	})

	return json.NewEncoder(w).Encode(statuses)
}

// handleError renders appropriate error responses based on request type
func (s *Server) handleError(w http.ResponseWriter, e error) {
	// Preserve CORS headers for API requests
	if CORS() {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, User-Agent, Accept, X-Requested-With")
		w.Header().Set("Access-Control-Allow-Credentials", "true")
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	json.NewEncoder(w).Encode(map[string]string{"error": e.Error()})
}

// userAgentTransport is a custom RoundTripper that adds the User-Agent header to all requests
type userAgentTransport struct {
	base http.RoundTripper
}

func (t *userAgentTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// Clone the request to avoid mutating the original
	r := req.Clone(req.Context())
	r.Header.Set("User-Agent", userAgent())
	return t.base.RoundTrip(r)
}

// httpClient returns an HTTP client that automatically adds the User-Agent header
func (s *Server) httpClient() *http.Client {
	return userAgentHTTPClient(10 * time.Second)
}

// inferenceClient uses almost the same HTTP client, but without a timeout so
// long requests aren't truncated
func (s *Server) inferenceClient() *api.Client {
	return api.NewClient(envconfig.Host(), userAgentHTTPClient(0))
}

func userAgentHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &userAgentTransport{
			base: http.DefaultTransport,
		},
	}
}

// doSelfSigned sends a self-signed request to the ollama.com API
func (s *Server) doSelfSigned(ctx context.Context, method, path string) (*http.Response, error) {
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	// Form the string to sign: METHOD,PATH?ts=TIMESTAMP
	signString := fmt.Sprintf("%s,%s?ts=%s", method, path, timestamp)
	signature, err := ollamaAuth.Sign(ctx, []byte(signString))
	if err != nil {
		return nil, fmt.Errorf("failed to sign request: %w", err)
	}

	endpoint := fmt.Sprintf("%s%s?ts=%s", OllamaDotCom, path, timestamp)
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", signature))

	return s.httpClient().Do(req)
}

// UserData fetches user data from ollama.com API for the current ollama key
func (s *Server) UserData(ctx context.Context) (*api.UserResponse, error) {
	resp, err := s.doSelfSigned(ctx, http.MethodPost, "/api/me")
	if err != nil {
		return nil, fmt.Errorf("failed to call ollama.com/api/me: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var user api.UserResponse
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("failed to parse user response: %w", err)
	}

	user.AvatarURL = fmt.Sprintf("%s/%s", OllamaDotCom, user.AvatarURL)

	storeUser := store.User{
		Name:  user.Name,
		Email: user.Email,
		Plan:  user.Plan,
	}
	if err := s.Store.SetUser(storeUser); err != nil {
		s.log().Warn("failed to cache user data", "error", err)
	}

	return &user, nil
}

// WaitForServer waits for the Ollama server to be ready
func WaitForServer(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := api.ClientFromEnvironment()
		if err != nil {
			return err
		}
		if _, err := c.Version(ctx); err == nil {
			slog.Debug("ollama server is ready")
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("timeout waiting for Ollama server to be ready")
}

func (s *Server) createChat(w http.ResponseWriter, r *http.Request) error {
	if err := WaitForServer(r.Context(), 10*time.Second); err != nil {
		return err
	}

	id, err := uuid.NewV7()
	if err != nil {
		return fmt.Errorf("failed to generate chat ID: %w", err)
	}

	json.NewEncoder(w).Encode(map[string]string{"id": id.String()})
	return nil
}

func (s *Server) listChats(w http.ResponseWriter, r *http.Request) error {
	chats, _ := s.Store.Chats()

	chatInfos := make([]responses.ChatInfo, len(chats))
	for i, chat := range chats {
		chatInfos[i] = chatInfoFromChat(chat)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(responses.ChatsResponse{ChatInfos: chatInfos})
	return nil
}

// checkModelUpstream makes a HEAD request to the Ollama registry to get the upstream digest and push time
func (s *Server) checkModelUpstream(ctx context.Context, modelName string, timeout time.Duration) (string, int64, error) {
	// Create a context with timeout for the registry check
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Parse model name to get namespace, model, and tag
	parts := strings.Split(modelName, ":")
	name := parts[0]
	tag := "latest"
	if len(parts) > 1 {
		tag = parts[1]
	}

	if !strings.Contains(name, "/") {
		// If the model name does not contain a slash, assume it's a library model
		name = "library/" + name
	}

	// Check the model in the Ollama registry using HEAD request
	url := OllamaDotCom + "/v2/" + name + "/manifests/" + tag
	req, err := http.NewRequestWithContext(checkCtx, "HEAD", url, nil)
	if err != nil {
		return "", 0, err
	}

	httpClient := s.httpClient()
	httpClient.Timeout = timeout

	resp, err := httpClient.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("registry returned status %d", resp.StatusCode)
	}

	digest := resp.Header.Get("ollama-content-digest")
	if digest == "" {
		return "", 0, fmt.Errorf("no digest header found")
	}

	var pushTime int64
	if pushTimeStr := resp.Header.Get("ollama-push-time"); pushTimeStr != "" {
		if pt, err := strconv.ParseInt(pushTimeStr, 10, 64); err == nil {
			pushTime = pt
		}
	}

	return digest, pushTime, nil
}

// isNetworkError checks if an error string contains common network/connection error patterns
func isNetworkError(errStr string) bool {
	networkErrorPatterns := []string{
		"connection refused",
		"no such host",
		"timeout",
		"network is unreachable",
		"connection reset",
		"connection timed out",
		"temporary failure",
		"dial tcp",
		"i/o timeout",
		"context deadline exceeded",
		"broken pipe",
	}

	for _, pattern := range networkErrorPatterns {
		if strings.Contains(errStr, pattern) {
			return true
		}
	}
	return false
}

var ErrNetworkOffline = errors.New("network is offline")

func (s *Server) getError(err error) responses.ErrorEvent {
	var sErr api.AuthorizationError
	if errors.As(err, &sErr) && sErr.StatusCode == http.StatusUnauthorized {
		return responses.ErrorEvent{
			EventName: "error",
			Error:     "Could not verify you are signed in. Please sign in and try again.",
			Code:      "cloud_unauthorized",
		}
	}

	errStr := err.Error()

	switch {
	case strings.Contains(errStr, "402"):
		return responses.ErrorEvent{
			EventName: "error",
			Error:     "You've reached your usage limit, please upgrade to continue",
			Code:      "usage_limit_upgrade",
		}
	case strings.HasPrefix(errStr, "pull model manifest") && isNetworkError(errStr):
		return responses.ErrorEvent{
			EventName: "error",
			Error:     "Unable to download model. Please check your internet connection to download the model for offline use.",
			Code:      "offline_download_error",
		}
	case errors.Is(err, ErrNetworkOffline) || strings.Contains(errStr, "operation timed out"):
		return responses.ErrorEvent{
			EventName: "error",
			Error:     "Connection lost",
			Code:      "turbo_connection_lost",
		}
	}
	return responses.ErrorEvent{
		EventName: "error",
		Error:     err.Error(),
	}
}

func userMessageText(messages []store.Message) string {
	var b strings.Builder
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		b.WriteString(message.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

func (s *Server) browserState(chat *store.Chat) (*responses.BrowserStateData, bool) {
	if len(chat.BrowserState) > 0 {
		var st responses.BrowserStateData
		if err := json.Unmarshal(chat.BrowserState, &st); err == nil {
			return &st, true
		}
	}
	return nil, false
}

// reconstructBrowserState (legacy): return the latest full browser state stored in messages.
func reconstructBrowserState(messages []store.Message, defaultViewTokens int) *responses.BrowserStateData {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.ToolResult == nil {
			continue
		}
		var st responses.BrowserStateData
		if err := json.Unmarshal(*msg.ToolResult, &st); err == nil {
			if len(st.PageStack) > 0 || len(st.URLToPage) > 0 {
				if st.ViewTokens == 0 {
					st.ViewTokens = defaultViewTokens
				}
				return &st
			}
		}
	}
	return nil
}

func (s *Server) chat(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "text/jsonl")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Transfer-Encoding", "chunked")

	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("streaming not supported")
	}

	if r.Method != "POST" {
		return not.Found
	}

	cid := r.PathValue("id")
	createdChat := false

	// if cid is the literal string "new", then we create a new chat before
	// performing our normal actions
	if cid == "new" {
		u, err := uuid.NewV7()
		if err != nil {
			return fmt.Errorf("failed to generate new chat id: %w", err)
		}
		cid = u.String()
		createdChat = true
	}

	var req responses.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fmt.Fprintf(os.Stderr, "error unmarshalling body: %v\n", err)
		return fmt.Errorf("invalid request body: %w", err)
	}

	if req.Model == "" {
		return fmt.Errorf("empty model")
	}
	traceID := strings.TrimSpace(req.TraceID)
	if traceID == "" {
		if id, err := uuid.NewV7(); err == nil {
			traceID = id.String()
		} else {
			traceID = fmt.Sprintf("chat-trace-%d", time.Now().UnixNano())
		}
	}

	// Don't allow empty messages unless forceUpdate is true
	if req.Prompt == "" && !req.ForceUpdate {
		return fmt.Errorf("empty message")
	}

	if createdChat {
		// send message to the client that the chat has been created
		json.NewEncoder(w).Encode(responses.ChatEvent{
			EventName: "chat_created",
			ChatID:    &cid,
		})
		flusher.Flush()
	}

	// Check if this is from a specific message index (e.g. for editing)
	idx := -1
	if req.Index != nil {
		idx = *req.Index
	}

	// Load chat with attachments since we need them for processing
	chat, err := s.Store.ChatWithOptions(cid, true)
	if err != nil {
		if !errors.Is(err, not.Found) {
			return err
		}
		chat = store.NewChat(cid)
	}
	s.log().Info("chat context trace: send received",
		"trace_id", traceID,
		"chat_id", cid,
		"model", req.Model,
		"force_update", req.ForceUpdate,
		"ux_estimated_context_tokens", req.UXEstimatedContextTokens,
		"canonical_turns_before_append", len(chat.Messages),
		"attachments", len(req.Attachments),
	)

	// Only add user message if not forceUpdate
	if !req.ForceUpdate {
		var messageOptions *store.MessageOptions
		if len(req.Attachments) > 0 {
			storeAttachments := make([]store.File, 0, len(req.Attachments))

			for _, att := range req.Attachments {
				if att.Data == "" {
					// This is an existing file reference - keep it from the original message
					if idx >= 0 && idx < len(chat.Messages) {
						originalMessage := chat.Messages[idx]
						// Find the file by filename in the original message
						for _, originalFile := range originalMessage.Attachments {
							if originalFile.Filename == att.Filename {
								storeAttachments = append(storeAttachments, originalFile)
								break
							}
						}
					}
				} else {
					// This is a new file - decode base64 data
					data, err := base64.StdEncoding.DecodeString(att.Data)
					if err != nil {
						s.log().Error("failed to decode attachment data", "error", err, "filename", att.Filename)
						continue
					}

					storeAttachments = append(storeAttachments, store.File{
						Filename: att.Filename,
						Data:     data,
					})
				}
			}

			messageOptions = &store.MessageOptions{
				Attachments: storeAttachments,
			}
		}
		userMsg := store.NewMessage("user", req.Prompt, messageOptions)

		if idx >= 0 && idx < len(chat.Messages) {
			// Generate from specified message: truncate and replace
			chat.Messages = chat.Messages[:idx]
			chat.Messages = append(chat.Messages, userMsg)
		} else {
			// Normal mode: append new message
			chat.Messages = append(chat.Messages, userMsg)
		}

		if err := s.Store.SetChat(*chat); err != nil {
			return err
		}
	}
	settingsForTrace, settingsErr := s.Store.Settings()
	if settingsErr != nil {
		s.log().Warn("chat context trace: settings unavailable", "trace_id", traceID, "chat_id", cid, "error", settingsErr)
	}
	configuredContextLength := 0
	if settingsErr == nil {
		configuredContextLength = settingsForTrace.ContextLength
	}
	canonicalEstimate := estimateMessagesTokens(chat.Messages)
	s.log().Info("chat context trace: canonical prepared",
		"trace_id", traceID,
		"chat_id", cid,
		"context_length", configuredContextLength,
		"ux_estimated_context_tokens", req.UXEstimatedContextTokens,
		"canonical_turns", len(chat.Messages),
		"canonical_estimated_tokens", canonicalEstimate,
	)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	_, cancelLoading := context.WithCancel(ctx)
	loading := false

	c := s.inferenceClient()

	// Check if the model exists locally by trying to show it
	// TODO (jmorganca): skip this round trip and instead just act
	// on a 404 error on chat
	_, err = c.Show(ctx, &api.ShowRequest{Model: req.Model})
	if err != nil || req.ForceUpdate {
		// Create an empty assistant message to store the model information
		// This will be overwritten when the model responds
		chat.Messages = append(chat.Messages, store.NewMessage("assistant", "", &store.MessageOptions{Model: req.Model}))
		if err := s.Store.SetChat(*chat); err != nil {
			cancelLoading()
			return err
		}
		// Send download progress events while the model is being pulled
		// TODO (jmorganca): this only shows the largest digest, but we
		// should show the progress for the total size of the download
		var largestDigest string
		var largestTotal int64

		err = c.Pull(ctx, &api.PullRequest{Model: req.Model}, func(progress api.ProgressResponse) error {
			if progress.Digest != "" && progress.Total > largestTotal {
				largestDigest = progress.Digest
				largestTotal = progress.Total
			}

			if progress.Digest != "" && progress.Digest == largestDigest {
				progressEvent := responses.DownloadEvent{
					EventName: string(EventDownload),
					Total:     progress.Total,
					Completed: progress.Completed,
					Done:      false,
				}

				if err := json.NewEncoder(w).Encode(progressEvent); err != nil {
					return err
				}
				flusher.Flush()
			}
			return nil
		})
		if err != nil {
			s.log().Error("model download error", "error", err, "model", req.Model)
			errorEvent := s.getError(err)
			json.NewEncoder(w).Encode(errorEvent)
			flusher.Flush()
			cancelLoading()
			return fmt.Errorf("failed to download model: %w", err)
		}

		if err := json.NewEncoder(w).Encode(responses.DownloadEvent{
			EventName: string(EventDownload),
			Completed: largestTotal,
			Total:     largestTotal,
			Done:      true,
		}); err != nil {
			cancelLoading()
			return err
		}
		flusher.Flush()

		// If forceUpdate, we're done after updating the model
		if req.ForceUpdate {
			json.NewEncoder(w).Encode(responses.ChatEvent{EventName: "done"})
			flusher.Flush()
			cancelLoading()
			return nil
		}
	}

	loading = true
	defer cancelLoading()

	// Check the model capabilities
	details, err := c.Show(ctx, &api.ShowRequest{Model: req.Model})

	if err != nil || details == nil {
		errorEvent := s.getError(err)
		json.NewEncoder(w).Encode(errorEvent)
		flusher.Flush()
		s.log().Error("failed to show model details", "error", err, "model", req.Model)
		return nil
	}
	think := slices.Contains(details.Capabilities, model.CapabilityThinking)

	var thinkValue any

	if req.Think != nil {
		thinkValue = req.Think
	} else {
		thinkValue = think
	}

	// Check if the last user message has attachments
	// TODO (parthsareen): this logic will change with directory drag and drop
	hasAttachments := false
	if len(chat.Messages) > 0 {
		lastMsg := chat.Messages[len(chat.Messages)-1]
		if lastMsg.Role == "user" && len(lastMsg.Attachments) > 0 {
			hasAttachments = true
		}
	}

	hasToolsCapability := slices.Contains(details.Capabilities, model.CapabilityTools)

	// Check if agent or tools mode is enabled.
	// Persistent plugin tools remain available to the host, but are only
	// granted to this Send when the caller explicitly enables file_tools.
	fileToolsEnabled := req.FileTools != nil && *req.FileTools
	registry := requestToolRegistry(s.ToolRegistry, hasToolsCapability, fileToolsEnabled)
	var browser *tools.Browser
	ctx = tools.WithAllowedDirectURLs(ctx, userMessageText(chat.Messages))

	if !hasAttachments {
		WebSearchEnabled := req.WebSearch != nil && *req.WebSearch

		if WebSearchEnabled && hasToolsCapability {
			if supportsBrowserTools(req.Model) {
				browserState, ok := s.browserState(chat)
				if !ok {
					browserState = reconstructBrowserState(chat.Messages, tools.DefaultViewTokens)
				}
				browser = tools.NewBrowser(browserState)
				registry.Register(tools.NewBrowserSearch(browser))
				registry.Register(tools.NewBrowserOpen(browser))
				registry.Register(tools.NewBrowserFind(browser))
			} else {
				registry.Register(&tools.WebSearch{})
				registry.Register(&tools.WebFetch{})
			}
		}
	}
	if hasToolsCapability {
		registerToolResultRecovery(registry, chat, configuredContextLength)
	}

	var thinkingTimeStart *time.Time = nil
	var thinkingTimeEnd *time.Time = nil
	// Request-only assistant tool_calls buffer
	// if tool_calls arrive before any assistant text, we keep them here,
	// inject them into the next request, and attach on first assistant content/thinking.
	var pendingAssistantToolCalls []store.ToolCall

	passNum := 1

	for {
		var toolsExecuted bool

		availableTools := registry.AvailableTools()

		// If we have pending assistant tool_calls and no assistant yet,
		// build the request against a temporary chat that includes a
		// request-only assistant with tool_calls inserted BEFORE tool messages
		reqChat := chat
		if len(pendingAssistantToolCalls) > 0 {
			if len(chat.Messages) == 0 || chat.Messages[len(chat.Messages)-1].Role != "assistant" {
				temp := *chat
				synth := store.NewMessage("assistant", "", &store.MessageOptions{Model: req.Model, ToolCalls: pendingAssistantToolCalls})
				insertIdx := len(temp.Messages) - 1
				for insertIdx >= 0 && temp.Messages[insertIdx].Role == "tool" {
					insertIdx--
				}
				if insertIdx < 0 {
					temp.Messages = append([]store.Message{synth}, temp.Messages...)
				} else {
					tmp := make([]store.Message, 0, len(temp.Messages)+1)
					tmp = append(tmp, temp.Messages[:insertIdx+1]...)
					tmp = append(tmp, synth)
					tmp = append(tmp, temp.Messages[insertIdx+1:]...)
					temp.Messages = tmp
				}

				reqChat = &temp
			}
		}
		compactionTrace := &contextCompactionTrace{}
		if shouldShowContextCompactionProgress(s, reqChat) {
			s.logContextCompactionTrigger(traceID, cid, passNum, req.UXEstimatedContextTokens, reqChat)
			if err := json.NewEncoder(w).Encode(responses.ChatEvent{
				EventName: "context_compaction",
				Content:   ptr("Compacting prior context into a checkpoint before continuing..."),
			}); err != nil {
				return err
			}
			flusher.Flush()
		}
		requestOverheadTokens := estimateToolDefinitionsTokens(availableTools)
		reqChat, err = s.operationalChatForRequestWithTraceAndOverhead(ctx, c, reqChat, req.Model, requestOverheadTokens, compactionTrace)
		if err != nil {
			s.log().Warn("chat context trace: compaction failed",
				"trace_id", traceID,
				"chat_id", cid,
				"pass", passNum,
				"context_length", compactionTrace.Limit,
				"canonical_turns", compactionTrace.CanonicalTurns,
				"canonical_estimated_tokens", compactionTrace.CanonicalEstimate,
				"triggered", compactionTrace.Triggered,
				"coverage", compactionTrace.Coverage,
				"error", err,
			)
			return err
		}
		s.log().Info("chat context trace: compaction observed",
			"trace_id", traceID,
			"chat_id", cid,
			"pass", passNum,
			"context_length", compactionTrace.Limit,
			"canonical_turns", compactionTrace.CanonicalTurns,
			"canonical_estimated_tokens", compactionTrace.CanonicalEstimate,
			"checkpoint_covered_turns", compactionTrace.CheckpointCoveredTurns,
			"projected_turns", compactionTrace.ProjectedTurns,
			"projected_estimated_tokens", compactionTrace.ProjectedEstimate,
			"triggered", compactionTrace.Triggered,
			"coverage", compactionTrace.Coverage,
			"delta_turns", compactionTrace.DeltaTurns,
			"before_tokens", compactionTrace.BeforeTokens,
			"after_tokens", compactionTrace.AfterTokens,
			"summary_tokens", compactionTrace.SummaryTokens,
			"summary_bytes", compactionTrace.SummaryBytes,
			"output_turns", compactionTrace.OutputTurns,
			"output_estimated_tokens", compactionTrace.OutputEstimate,
			"fallback", compactionTrace.Fallback,
			"error", compactionTrace.Error,
		)
		if compactionTrace.Triggered || compactionTrace.Fallback == "checkpoint_projection" {
			status := "Context checkpoint ready. Continuing with checkpoint + recent turns..."
			if compactionTrace.Fallback == "checkpoint_projection" {
				status = "Using existing context checkpoint + recent turns..."
			}
			if err := json.NewEncoder(w).Encode(responses.ChatEvent{
				EventName: "context_compaction",
				Content:   ptr(status),
			}); err != nil {
				return err
			}
			flusher.Flush()
		}
		chatReq, err := s.buildChatRequest(reqChat, req.Model, thinkValue, availableTools)
		if err != nil {
			return err
		}
		chatReq.TraceID = traceID
		chatReq.UXEstimatedContextTokens = req.UXEstimatedContextTokens
		finalRequestEstimate := estimateAPIChatRequestTokens(chatReq)
		finalRequestBudget := contextFinalRequestBudget(configuredContextLength)
		if configuredContextLength > 0 && finalRequestEstimate > finalRequestBudget {
			return fmt.Errorf("final chat request estimate %d exceeds input budget %d for configured context %d", finalRequestEstimate, finalRequestBudget, configuredContextLength)
		}
		s.log().Info("chat context trace: runtime request built",
			"trace_id", traceID,
			"chat_id", cid,
			"pass", passNum,
			"request_messages", len(chatReq.Messages),
			"request_tools", len(chatReq.Tools),
			"request_images", countAPIMessageImages(chatReq.Messages),
			"request_estimated_tokens", finalRequestEstimate,
			"request_input_budget", finalRequestBudget,
			"think", chatReq.Think != nil,
		)

		err = c.Chat(ctx, chatReq, func(res api.ChatResponse) error {
			if res.Done {
				s.log().Info("chat context trace: runtime response done",
					"trace_id", traceID,
					"chat_id", cid,
					"pass", passNum,
					"prompt_eval_count", res.PromptEvalCount,
					"eval_count", res.EvalCount,
					"finish_reason", res.DoneReason,
					"total_duration_ms", res.TotalDuration.Milliseconds(),
					"load_duration_ms", res.LoadDuration.Milliseconds(),
				)
			}
			if loading {
				// Remove the loading indicator on first token
				cancelLoading()
				loading = false
			}

			// Start thinking timer on first thinking content or after tool call when thinking again
			if res.Message.Thinking != "" && (thinkingTimeStart == nil || thinkingTimeEnd != nil) {
				now := time.Now()
				thinkingTimeStart = &now
				thinkingTimeEnd = nil
			}

			if res.Message.Content == "" && res.Message.Thinking == "" && len(res.Message.ToolCalls) == 0 {
				return nil
			}

			event := EventChat
			if thinkingTimeStart != nil && res.Message.Content == "" && len(res.Message.ToolCalls) == 0 {
				event = EventThinking
			}

			if len(res.Message.ToolCalls) > 0 {
				event = EventToolCall
			}

			if event == EventToolCall && thinkingTimeStart != nil && thinkingTimeEnd == nil {
				now := time.Now()
				thinkingTimeEnd = &now
			}

			if event == EventChat && thinkingTimeStart != nil && thinkingTimeEnd == nil && res.Message.Content != "" {
				now := time.Now()
				thinkingTimeEnd = &now
			}

			json.NewEncoder(w).Encode(chatEventFromApiChatResponse(res, thinkingTimeStart, thinkingTimeEnd))
			flusher.Flush()

			switch event {
			case EventToolCall:
				if thinkingTimeEnd != nil {
					if len(chat.Messages) > 0 && chat.Messages[len(chat.Messages)-1].Role == "assistant" {
						lastMsg := &chat.Messages[len(chat.Messages)-1]
						lastMsg.ThinkingTimeEnd = thinkingTimeEnd
						lastMsg.UpdatedAt = time.Now()
						s.Store.UpdateLastMessage(chat.ID, *lastMsg)
					}
					thinkingTimeStart = nil
					thinkingTimeEnd = nil
				}

				// attach tool_calls to an existing assistant if present,
				// otherwise (for standalone web_search/web_fetch) buffer for request-only injection.
				if len(res.Message.ToolCalls) > 0 {
					if len(chat.Messages) > 0 && chat.Messages[len(chat.Messages)-1].Role == "assistant" {
						toolCalls := make([]store.ToolCall, len(res.Message.ToolCalls))
						for i, tc := range res.Message.ToolCalls {
							argsJSON, _ := json.Marshal(tc.Function.Arguments)
							toolCalls[i] = store.ToolCall{
								Type: "function",
								Function: store.ToolFunction{
									Name:      tc.Function.Name,
									Arguments: string(argsJSON),
								},
							}
						}
						lastMsg := &chat.Messages[len(chat.Messages)-1]
						lastMsg.ToolCalls = toolCalls
						if err := s.Store.UpdateLastMessage(chat.ID, *lastMsg); err != nil {
							return err
						}
					} else {
						onlyStandalone := true
						for _, tc := range res.Message.ToolCalls {
							if !(tc.Function.Name == "web_search" || tc.Function.Name == "web_fetch") {
								onlyStandalone = false
								break
							}
						}
						if onlyStandalone {
							toolCalls := make([]store.ToolCall, len(res.Message.ToolCalls))
							for i, tc := range res.Message.ToolCalls {
								argsJSON, _ := json.Marshal(tc.Function.Arguments)
								toolCalls[i] = store.ToolCall{
									Type: "function",
									Function: store.ToolFunction{
										Name:      tc.Function.Name,
										Arguments: string(argsJSON),
									},
								}
							}

							synth := store.NewMessage("assistant", "", &store.MessageOptions{Model: req.Model, ToolCalls: toolCalls})
							chat.Messages = append(chat.Messages, synth)
							if err := s.Store.AppendMessage(chat.ID, synth); err != nil {
								return err
							}

							// clear buffer to avoid-injecting again
							pendingAssistantToolCalls = nil
						}
					}
				}

				for _, toolCall := range res.Message.ToolCalls {
					// continues loop as tools were executed
					toolsExecuted = true
					result, content, err := registry.Execute(ctx, toolCall.Function.Name, toolCall.Function.Arguments.ToMap())
					if err != nil {
						errContent := fmt.Sprintf("Error: %v", err)
						modelContent := content
						if modelContent == "" {
							modelContent = errContent
						} else {
							modelContent = errContent + "\n" + modelContent
						}
						var tr json.RawMessage
						if result != nil {
							resultJSON, marshalErr := json.Marshal(result)
							if marshalErr != nil {
								return fmt.Errorf("failed to marshal failed tool result: %w", marshalErr)
							}
							tr = resultJSON
						}
						var toolErrOptions *store.MessageOptions
						if len(tr) > 0 {
							toolErrOptions = &store.MessageOptions{ToolResult: &tr}
						}
						toolErrMsg := store.NewMessage("tool", modelContent, toolErrOptions)
						toolErrMsg.ToolName = toolCall.Function.Name
						chat.Messages = append(chat.Messages, toolErrMsg)
						if err := s.Store.AppendMessage(chat.ID, toolErrMsg); err != nil {
							return err
						}
						registerToolResultRecovery(registry, chat, configuredContextLength)

						// Emit tool error event
						toolResult := true
						json.NewEncoder(w).Encode(responses.ChatEvent{
							EventName: "tool",
							Content:   &modelContent,
							ToolName:  &toolCall.Function.Name,
						})
						flusher.Flush()

						json.NewEncoder(w).Encode(responses.ChatEvent{
							EventName:      "tool_result",
							Content:        &modelContent,
							ToolName:       &toolCall.Function.Name,
							ToolResult:     &toolResult,
							ToolResultData: result,
						})
						flusher.Flush()
						continue
					}

					var tr json.RawMessage
					if strings.HasPrefix(toolCall.Function.Name, "browser.search") {
						// For standalone web_search, ensure the tool message has readable content
						// so the second-pass model can consume results, while keeping browser state flow intact.
						// We still persist tool msg with content below.
						// (No browser state update needed for standalone.)
					} else if strings.HasPrefix(toolCall.Function.Name, "browser") {
						stateBytes, err := json.Marshal(browser.State())
						if err != nil {
							return fmt.Errorf("failed to marshal browser state: %w", err)
						}
						if err := s.Store.UpdateChatBrowserState(chat.ID, json.RawMessage(stateBytes)); err != nil {
							return fmt.Errorf("failed to persist browser state to chat: %w", err)
						}
						// tool result is not added to the tool message for the browser tool
					} else {
						var err error
						tr, err = json.Marshal(result)
						if err != nil {
							return fmt.Errorf("failed to marshal tool result: %w", err)
						}
					}
					// ensure tool message sent back to the model has content (if empty, use a sensible fallback)
					modelContent := content
					if toolCall.Function.Name == "web_fetch" && modelContent == "" {
						if str, ok := result.(string); ok {
							modelContent = str
						}
					}
					if modelContent == "" && len(tr) > 0 {
						s.log().Debug("tool message empty, sending json result")
						modelContent = string(tr)
					}
					toolMsg := store.NewMessage("tool", modelContent, &store.MessageOptions{
						ToolResult: &tr,
					})
					toolMsg.ToolName = toolCall.Function.Name
					chat.Messages = append(chat.Messages, toolMsg)

					if err := s.Store.AppendMessage(chat.ID, toolMsg); err != nil {
						return err
					}
					registerToolResultRecovery(registry, chat, configuredContextLength)

					// Emit tool message event (matching agent pattern)
					toolResult := true
					json.NewEncoder(w).Encode(responses.ChatEvent{
						EventName: "tool",
						Content:   &content,
						ToolName:  &toolCall.Function.Name,
					})
					flusher.Flush()

					var toolState any = nil
					if browser != nil {
						toolState = browser.State()
					}
					// Stream tool result to frontend

					json.NewEncoder(w).Encode(responses.ChatEvent{
						EventName:      "tool_result",
						Content:        &content,
						ToolName:       &toolCall.Function.Name,
						ToolResult:     &toolResult,
						ToolResultData: result,
						ToolState:      toolState,
					})
					flusher.Flush()
				}

			case EventChat:
				// Append the new message to the chat history
				if len(chat.Messages) == 0 || chat.Messages[len(chat.Messages)-1].Role != "assistant" {
					newMsg := store.NewMessage("assistant", "", &store.MessageOptions{Model: req.Model})
					chat.Messages = append(chat.Messages, newMsg)
					// Append new message to database
					if err := s.Store.AppendMessage(chat.ID, newMsg); err != nil {
						return err
					}
					// Attach any buffered tool_calls (request-only) now that assistant has started
					if len(pendingAssistantToolCalls) > 0 {
						lastMsg := &chat.Messages[len(chat.Messages)-1]
						lastMsg.ToolCalls = pendingAssistantToolCalls

						pendingAssistantToolCalls = nil
						if err := s.Store.UpdateLastMessage(chat.ID, *lastMsg); err != nil {
							return err
						}
					}
				}

				// Append token to last assistant message & persist
				lastMsg := &chat.Messages[len(chat.Messages)-1]
				lastMsg.Content += res.Message.Content
				lastMsg.UpdatedAt = time.Now()
				// Update thinking time fields
				if thinkingTimeStart != nil {
					lastMsg.ThinkingTimeStart = thinkingTimeStart
				}
				if thinkingTimeEnd != nil {
					lastMsg.ThinkingTimeEnd = thinkingTimeEnd
				}
				// Use optimized update for streaming
				if err := s.Store.UpdateLastMessage(chat.ID, *lastMsg); err != nil {
					return err
				}
			case EventThinking:
				// Persist thinking content
				if len(chat.Messages) == 0 || chat.Messages[len(chat.Messages)-1].Role != "assistant" {
					newMsg := store.NewMessage("assistant", "", &store.MessageOptions{
						Model:    req.Model,
						Thinking: res.Message.Thinking,
					})
					chat.Messages = append(chat.Messages, newMsg)
					// Append new message to database
					if err := s.Store.AppendMessage(chat.ID, newMsg); err != nil {
						return err
					}
					// Attach any buffered tool_calls now that assistant exists
					if len(pendingAssistantToolCalls) > 0 {
						lastMsg := &chat.Messages[len(chat.Messages)-1]
						lastMsg.ToolCalls = pendingAssistantToolCalls

						pendingAssistantToolCalls = nil
						if err := s.Store.UpdateLastMessage(chat.ID, *lastMsg); err != nil {
							return err
						}
					}
				} else {
					// Update thinking content of existing message
					lastMsg := &chat.Messages[len(chat.Messages)-1]
					lastMsg.Thinking += res.Message.Thinking
					lastMsg.UpdatedAt = time.Now()
					// Update thinking time fields
					if thinkingTimeStart != nil {
						lastMsg.ThinkingTimeStart = thinkingTimeStart
					}
					if thinkingTimeEnd != nil {
						lastMsg.ThinkingTimeEnd = thinkingTimeEnd
					}

					// Use optimized update for streaming
					if err := s.Store.UpdateLastMessage(chat.ID, *lastMsg); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			s.log().Error("chat stream error", "error", err)
			errorEvent := s.getError(err)
			json.NewEncoder(w).Encode(errorEvent)
			flusher.Flush()
			return nil
		}

		// If no tools were executed, exit the loop
		if !toolsExecuted {
			break
		}
		passNum++
	}

	// handle cases where thinking started but didn't finish
	// this can happen if the client disconnects or the request is cancelled
	// TODO (jmorganca): this should be merged with code above
	if thinkingTimeStart != nil && thinkingTimeEnd == nil {
		now := time.Now()
		thinkingTimeEnd = &now
		if len(chat.Messages) > 0 && chat.Messages[len(chat.Messages)-1].Role == "assistant" {
			lastMsg := &chat.Messages[len(chat.Messages)-1]
			lastMsg.ThinkingTimeEnd = thinkingTimeEnd
			lastMsg.UpdatedAt = time.Now()
			s.Store.UpdateLastMessage(chat.ID, *lastMsg)
		}
	}

	json.NewEncoder(w).Encode(responses.ChatEvent{EventName: "done"})
	flusher.Flush()

	if len(chat.Messages) > 0 {
		chat.Messages[len(chat.Messages)-1].Stream = false
	}
	return s.Store.SetChat(*chat)
}

func (s *Server) getChat(w http.ResponseWriter, r *http.Request) error {
	cid := r.PathValue("id")

	if cid == "" {
		return fmt.Errorf("chat ID is required")
	}

	chat, err := s.Store.Chat(cid)
	if err != nil {
		// Return empty chat if not found
		data := responses.ChatResponse{
			Chat: store.Chat{},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(data)
		return nil //nolint:nilerr
	}

	// fill missing tool_name on tool messages (from previous tool_calls) so labels don’t flip after reload.
	if chat != nil && len(chat.Messages) > 0 {
		for i := range chat.Messages {
			if chat.Messages[i].Role == "tool" && chat.Messages[i].ToolName == "" && chat.Messages[i].ToolResult != nil {
				for j := i - 1; j >= 0; j-- {
					if chat.Messages[j].Role == "assistant" && len(chat.Messages[j].ToolCalls) > 0 {
						last := chat.Messages[j].ToolCalls[len(chat.Messages[j].ToolCalls)-1]
						if last.Function.Name != "" {
							chat.Messages[i].ToolName = last.Function.Name
						}
						break
					}
				}
			}
		}
	}

	browserState, ok := s.browserState(chat)
	if !ok {
		browserState = reconstructBrowserState(chat.Messages, tools.DefaultViewTokens)
	}
	// clear the text and lines of all pages as it is not needed for rendering
	if browserState != nil {
		for _, page := range browserState.URLToPage {
			page.Lines = nil
			page.Text = ""
		}

		if cleanedState, err := json.Marshal(browserState); err == nil {
			chat.BrowserState = json.RawMessage(cleanedState)
		}
	}
	checkpoint, err := s.Store.ContextCheckpoint(chat.ID, chat.Messages)
	if err != nil {
		s.log().Warn("context checkpoint unavailable for chat response", "chat_id", chat.ID, "error", err)
	}
	var checkpointInfo *responses.ContextCheckpointInfo
	if checkpoint != nil {
		checkpointInfo = &responses.ContextCheckpointInfo{
			CoveredMessageCount:   checkpoint.CoveredMessageCount,
			EstimatedTokensBefore: checkpoint.EstimatedTokensBefore,
			EstimatedTokensAfter:  checkpoint.EstimatedTokensAfter,
			CreatedAt:             checkpoint.CreatedAt,
		}
	}
	data := responses.ChatResponse{
		Chat:              *chat,
		ContextCheckpoint: checkpointInfo,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
	return nil
}

func (s *Server) renameChat(w http.ResponseWriter, r *http.Request) error {
	cid := r.PathValue("id")
	if cid == "" {
		return fmt.Errorf("chat ID is required")
	}

	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	// Get the chat without loading attachments (we only need to update the title)
	chat, err := s.Store.ChatWithOptions(cid, false)
	if err != nil {
		return fmt.Errorf("chat not found: %w", err)
	}

	// Update the title
	chat.Title = req.Title
	if err := s.Store.SetChat(*chat); err != nil {
		return fmt.Errorf("failed to update chat: %w", err)
	}

	// Return the updated chat info
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(chatInfoFromChat(*chat))
	return nil
}

func (s *Server) deleteChat(w http.ResponseWriter, r *http.Request) error {
	cid := r.PathValue("id")
	if cid == "" {
		return fmt.Errorf("chat ID is required")
	}

	// Check if the chat exists (no need to load attachments)
	_, err := s.Store.ChatWithOptions(cid, false)
	if err != nil {
		if errors.Is(err, not.Found) {
			w.WriteHeader(http.StatusNotFound)
			return fmt.Errorf("chat not found")
		}
		return fmt.Errorf("failed to get chat: %w", err)
	}

	// Delete the chat
	if err := s.Store.DeleteChat(cid); err != nil {
		return fmt.Errorf("failed to delete chat: %w", err)
	}

	w.WriteHeader(http.StatusOK)
	return nil
}

// TODO(parthsareen): consolidate events within the function
func chatEventFromApiChatResponse(res api.ChatResponse, thinkingTimeStart *time.Time, thinkingTimeEnd *time.Time) responses.ChatEvent {
	// If there are tool calls, send assistant_with_tools event
	if len(res.Message.ToolCalls) > 0 {
		// Convert API tool calls to store tool calls
		storeToolCalls := make([]store.ToolCall, len(res.Message.ToolCalls))
		for i, tc := range res.Message.ToolCalls {
			argsJSON, _ := json.Marshal(tc.Function.Arguments)
			storeToolCalls[i] = store.ToolCall{
				Type: "function",
				Function: store.ToolFunction{
					Name:      tc.Function.Name,
					Arguments: string(argsJSON),
				},
			}
		}

		var content *string
		if res.Message.Content != "" {
			content = &res.Message.Content
		}
		var thinking *string
		if res.Message.Thinking != "" {
			thinking = &res.Message.Thinking
		}

		return responses.ChatEvent{
			EventName:         "assistant_with_tools",
			Content:           content,
			Thinking:          thinking,
			ToolCalls:         storeToolCalls,
			ThinkingTimeStart: thinkingTimeStart,
			ThinkingTimeEnd:   thinkingTimeEnd,
		}
	}

	// Otherwise, send regular chat event
	var content *string
	if res.Message.Content != "" {
		content = &res.Message.Content
	}
	var thinking *string
	if res.Message.Thinking != "" {
		thinking = &res.Message.Thinking
	}

	return responses.ChatEvent{
		EventName:         "chat",
		Content:           content,
		Thinking:          thinking,
		ThinkingTimeStart: thinkingTimeStart,
		ThinkingTimeEnd:   thinkingTimeEnd,
	}
}

func chatInfoFromChat(chat store.Chat) responses.ChatInfo {
	userExcerpt := ""
	var updatedAt time.Time

	for _, msg := range chat.Messages {
		// extract the first user message as the user excerpt
		if msg.Role == "user" && userExcerpt == "" {
			userExcerpt = msg.Content
		}
		// update the updated at time
		if msg.UpdatedAt.After(updatedAt) {
			updatedAt = msg.UpdatedAt
		}
	}

	return responses.ChatInfo{
		ID:          chat.ID,
		Title:       chat.Title,
		UserExcerpt: userExcerpt,
		CreatedAt:   chat.CreatedAt,
		UpdatedAt:   updatedAt,
	}
}

func shouldShowContextCompactionProgress(s *Server, chat *store.Chat) bool {
	settings, err := s.Store.Settings()
	if err != nil || settings.ContextLength <= 0 {
		return false
	}

	trigger := int(float64(settings.ContextLength) * contextCompactionTriggerRatio)
	if trigger <= 0 || estimateMessagesTokens(chat.Messages) < trigger {
		return false
	}

	checkpoint, err := s.Store.ContextCheckpoint(chat.ID, chat.Messages)
	if err != nil {
		return false
	}
	if checkpoint != nil {
		projected := projectChatForRequest(chat, checkpoint, trigger)
		if estimateMessagesTokens(projected.Messages) < trigger {
			return false
		}
	}

	coverage := chooseContextCoverage(chat.Messages, int(float64(settings.ContextLength)*contextCompactionTargetRatio))
	if coverage <= 0 {
		return false
	}
	return checkpoint == nil || coverage > checkpoint.CoveredMessageCount
}

func (s *Server) logContextCompactionTrigger(traceID, chatID string, pass, uxActiveTokens int, chat *store.Chat) {
	settings, err := s.Store.Settings()
	if err != nil || settings.ContextLength <= 0 {
		return
	}

	canonicalEstimate := estimateMessagesTokens(chat.Messages)
	activeEstimate := canonicalEstimate
	checkpointCoveredTurns := 0
	checkpoint, checkpointErr := s.Store.ContextCheckpoint(chat.ID, chat.Messages)
	if checkpointErr == nil && checkpoint != nil {
		checkpointCoveredTurns = checkpoint.CoveredMessageCount
		activeEstimate = estimateMessagesTokens(projectChatForRequest(chat, checkpoint, int(float64(settings.ContextLength)*contextCompactionTriggerRatio)).Messages)
	}

	imageTokens, imageBytes, imageCount := estimateImageAttachmentTokens(chat.Messages)
	latestImageTokens, latestImageBytes, latestImageCount := 0, 0, 0
	if len(chat.Messages) > 0 {
		latestImageTokens, latestImageBytes, latestImageCount = estimateImageAttachmentTokens(chat.Messages[len(chat.Messages)-1:])
	}

	s.log().Info("chat context trace: compaction progress triggered",
		"trace_id", traceID,
		"chat_id", chatID,
		"pass", pass,
		"context_length", settings.ContextLength,
		"threshold_tokens", int(float64(settings.ContextLength)*contextCompactionTriggerRatio),
		"active_context_tokens", activeEstimate,
		"ux_active_context_tokens", uxActiveTokens,
		"canonical_estimated_tokens", canonicalEstimate,
		"image_estimated_tokens", imageTokens,
		"image_bytes", imageBytes,
		"image_count", imageCount,
		"latest_turn_image_estimated_tokens", latestImageTokens,
		"latest_turn_image_bytes", latestImageBytes,
		"latest_turn_image_count", latestImageCount,
		"checkpoint_covered_turns", checkpointCoveredTurns,
		"checkpoint_error", checkpointErr,
	)
}

func estimateImageAttachmentTokens(messages []store.Message) (tokens, bytes, count int) {
	for _, msg := range messages {
		for _, attachment := range msg.Attachments {
			if !isImageAttachment(attachment.Filename) {
				continue
			}
			tokens += estimateTokens(attachment.Filename) + api.ImageTokenEstimate
			bytes += len(attachment.Data)
			count++
		}
	}
	return tokens, bytes, count
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) error {
	settings, err := s.Store.Settings()
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	// set default models directory if not set
	if settings.Models == "" {
		settings.Models = envconfig.Models()
	}

	// Include current runtime settings
	settings.Agent = s.Agent
	settings.WorkingDir = s.WorkingDir

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(responses.SettingsResponse{
		Settings: settings,
	})
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) error {
	old, err := s.Store.Settings()
	if err != nil {
		return fmt.Errorf("failed to load settings: %w", err)
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return fmt.Errorf("failed to read request body: %w", err)
	}

	var settings store.Settings
	if err := json.Unmarshal(body, &settings); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	var rawFields map[string]json.RawMessage
	if err := json.Unmarshal(body, &rawFields); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	if _, ok := rawFields["OnboardingVersion"]; !ok {
		settings.OnboardingVersion = old.OnboardingVersion
	}
	if _, ok := rawFields["ClaudeDesktopUsed"]; !ok {
		settings.ClaudeDesktopUsed = old.ClaudeDesktopUsed
	}
	if _, ok := rawFields["PluginsEnabled"]; !ok {
		settings.PluginsEnabled = old.PluginsEnabled
	}
	if _, ok := rawFields["LocalModelsEnabled"]; !ok {
		settings.LocalModelsEnabled = old.LocalModelsEnabled
	}
	if _, ok := rawFields["CPUFallbackEnabled"]; !ok {
		settings.CPUFallbackEnabled = old.CPUFallbackEnabled
	}
	if _, ok := rawFields["InferenceEnabled"]; !ok {
		settings.InferenceEnabled = old.InferenceEnabled
	}
	if _, ok := rawFields["InferenceRuntime"]; !ok {
		settings.InferenceRuntime = old.InferenceRuntime
	}
	if _, ok := rawFields["InferenceAutoFitEnabled"]; !ok {
		settings.InferenceAutoFitEnabled = old.InferenceAutoFitEnabled
	}
	if _, ok := rawFields["InferenceManualLayers"]; !ok {
		settings.InferenceManualLayers = old.InferenceManualLayers
	}

	if err := s.Store.SetSettings(settings); err != nil {
		return fmt.Errorf("failed to save settings: %w", err)
	}

	// Handle auto-update toggle changes
	if old.AutoUpdateEnabled != settings.AutoUpdateEnabled {
		if !settings.AutoUpdateEnabled {
			// Auto-update disabled: cancel any ongoing download
			if s.Updater != nil {
				s.Updater.CancelOngoingDownload()
			}
		} else {
			// Auto-update re-enabled: show notification if update is already staged, or trigger immediate check
			if (updater.IsUpdatePending() || updater.UpdateDownloaded) && s.UpdateAvailableFunc != nil {
				s.UpdateAvailableFunc()
			} else if s.Updater != nil {
				// Trigger the background checker to run immediately
				s.Updater.TriggerImmediateCheck()
			}
		}
	}

	if old.ContextLength != settings.ContextLength ||
		old.Models != settings.Models ||
		old.Expose != settings.Expose ||
		old.InferenceRuntime != settings.InferenceRuntime ||
		old.InferenceAutoFitEnabled != settings.InferenceAutoFitEnabled ||
		old.InferenceManualLayers != settings.InferenceManualLayers {
		s.Restart()
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(responses.SettingsResponse{
		Settings: settings,
	})
}

type managedModelsResponse struct {
	Models []managedModel `json:"models"`
}

type managedModel struct {
	Name        string           `json:"name"`
	Size        int64            `json:"size"`
	Digest      string           `json:"digest"`
	ModifiedAt  time.Time        `json:"modified_at"`
	Details     api.ModelDetails `json:"details,omitempty"`
	Origin      string           `json:"origin"`
	SourcePath  string           `json:"sourcePath,omitempty"`
	DisplayName string           `json:"displayName,omitempty"`
	CanRename   bool             `json:"canRename"`
	CanUnlink   bool             `json:"canUnlink"`
	CanDelete   bool             `json:"canDelete"`
	CanUpdate   bool             `json:"canUpdate"`
}

func (s *Server) listManagedModels(w http.ResponseWriter, r *http.Request) error {
	list, err := s.inferenceClient().List(r.Context())
	if err != nil {
		return fmt.Errorf("list models: %w", err)
	}

	metadataItems, err := s.Store.ModelMetadata()
	if err != nil {
		return fmt.Errorf("load model metadata: %w", err)
	}
	metadataByName := make(map[string]store.ModelMetadata, len(metadataItems))
	for _, item := range metadataItems {
		metadataByName[item.ModelName] = item
	}

	models := make([]managedModel, 0, len(list.Models))
	for _, item := range list.Models {
		if item.Digest == "" {
			continue
		}
		meta := metadataByName[item.Model]
		origin := meta.Origin
		if origin == "" {
			origin = "ollama_storage"
		}
		isLinked := origin == "linked_gguf"
		models = append(models, managedModel{
			Name:        item.Model,
			Size:        item.Size,
			Digest:      item.Digest,
			ModifiedAt:  item.ModifiedAt,
			Details:     item.Details,
			Origin:      origin,
			SourcePath:  meta.SourcePath,
			DisplayName: meta.DisplayName,
			CanRename:   true,
			CanUnlink:   isLinked,
			CanDelete:   !isLinked,
			CanUpdate:   !isLinked && item.RemoteHost != "",
		})
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(managedModelsResponse{Models: models})
}

func (s *Server) importModelFiles(w http.ResponseWriter, r *http.Request) error {
	return s.linkModelFiles(w, r)
}

func (s *Server) linkModelFiles(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Name  string   `json:"name"`
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		return fmt.Errorf("model name is required")
	}
	if parsed := model.ParseName(name); !parsed.IsValid() {
		return fmt.Errorf("invalid model name")
	}
	if len(req.Paths) == 0 {
		return fmt.Errorf("at least one GGUF file is required")
	}

	parsedName := model.ParseName(name)
	modelName := parsedName.DisplayShortest()
	s.log().Info("linking GGUF model", "model", modelName, "file_count", len(req.Paths))

	settings, err := s.Store.Settings()
	if err != nil {
		return fmt.Errorf("load model location: %w", err)
	}
	modelRoot := settings.Models
	if modelRoot == "" {
		modelRoot = envconfig.Models()
	}

	type linkedModelFile struct {
		path    string
		digest  string
		layer   manifest.Layer
		decoded *ggml.GGML
	}

	var baseFile *linkedModelFile
	projectorFiles := make([]linkedModelFile, 0, len(req.Paths)-1)
	var sourcePath string
	var modelFile *ggml.GGML
	for _, p := range req.Paths {
		if strings.TrimSpace(p) == "" {
			return fmt.Errorf("model file path is required")
		}

		realPath, err := filepath.EvalSymlinks(p)
		if err != nil {
			return fmt.Errorf("resolve model file %q: %w", filepath.Base(p), err)
		}
		info, err := os.Stat(realPath)
		if err != nil {
			return fmt.Errorf("read model file %q: %w", filepath.Base(p), err)
		}
		if info.IsDir() {
			return fmt.Errorf("model file %q is a directory", filepath.Base(p))
		}
		if !strings.EqualFold(filepath.Ext(realPath), ".gguf") {
			return fmt.Errorf("model file %q must be a .gguf file", filepath.Base(p))
		}

		digest, err := digestForImportFile(realPath)
		if err != nil {
			return fmt.Errorf("hash model file %q: %w", filepath.Base(p), err)
		}

		layer, decoded, err := linkGGUFBlob(modelRoot, realPath, digest)
		if err != nil {
			return fmt.Errorf("link model file %q: %w", filepath.Base(p), err)
		}

		linked := linkedModelFile{path: realPath, digest: digest, layer: layer, decoded: decoded}
		if isGGUFProjector(decoded) {
			layer.MediaType = "application/vnd.ollama.image.projector"
			linked.layer = layer
			projectorFiles = append(projectorFiles, linked)
			continue
		}
		if baseFile != nil {
			return fmt.Errorf("multiple base GGUF model files selected: %q and %q", filepath.Base(baseFile.path), filepath.Base(realPath))
		}
		baseFile = &linked
	}

	if baseFile == nil {
		return fmt.Errorf("base GGUF model file is required; selected files only include projector/mmproj files")
	}
	sourcePath = baseFile.path
	modelFile = baseFile.decoded
	layers := make([]manifest.Layer, 0, 1+len(projectorFiles))
	layers = append(layers, baseFile.layer)
	for _, projector := range projectorFiles {
		layers = append(layers, projector.layer)
	}

	configLayer, err := linkedGGUFConfigLayer(modelRoot, modelFile)
	if err != nil {
		return fmt.Errorf("create linked model config: %w", err)
	}

	if err := writeLinkedManifest(modelRoot, parsedName, *configLayer, layers); err != nil {
		return fmt.Errorf("write linked model manifest: %w", err)
	}

	if err := s.Store.UpsertModelMetadata(store.ModelMetadata{
		ModelName:  modelName,
		Origin:     "linked_gguf",
		SourcePath: sourcePath,
	}); err != nil {
		return fmt.Errorf("save linked model metadata: %w", err)
	}

	s.log().Info("linked GGUF model", "model", modelName, "file_count", len(layers))

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{
		"model": modelName,
	})
}

func linkGGUFBlob(modelRoot, path, digest string) (manifest.Layer, *ggml.GGML, error) {
	blobPath, err := linkedBlobPath(modelRoot, digest)
	if err != nil {
		return manifest.Layer{}, nil, err
	}

	if _, err := os.Stat(blobPath); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(blobPath), 0o755); err != nil {
			return manifest.Layer{}, nil, err
		}
		if err := os.Link(path, blobPath); err != nil {
			return manifest.Layer{}, nil, fmt.Errorf("create hardlink: %w", err)
		}
		if err := os.Chmod(blobPath, 0o644); err != nil {
			return manifest.Layer{}, nil, err
		}
	} else if err != nil {
		return manifest.Layer{}, nil, err
	}

	fi, err := os.Stat(blobPath)
	if err != nil {
		return manifest.Layer{}, nil, err
	}

	f, err := os.Open(blobPath)
	if err != nil {
		return manifest.Layer{}, nil, err
	}
	defer f.Close()
	decoded, err := ggml.Decode(f, -1)
	if err != nil {
		return manifest.Layer{}, nil, err
	}

	return manifest.Layer{
		MediaType: "application/vnd.ollama.image.model",
		Digest:    digest,
		Size:      fi.Size(),
		Status:    fmt.Sprintf("using existing layer %s", digest),
	}, decoded, nil
}

func isGGUFProjector(f *ggml.GGML) bool {
	kv := f.KV()
	kind := strings.ToLower(kv.Kind())
	arch := strings.ToLower(kv.Architecture())
	if kind == "projector" || kind == "mmproj" {
		return true
	}
	return arch == "clip"
}

func linkedBlobPath(modelRoot, digest string) (string, error) {
	if digest == "" {
		path := filepath.Join(modelRoot, "blobs")
		if err := os.MkdirAll(path, 0o755); err != nil {
			return "", fmt.Errorf("%w: ensure path elements are traversable", err)
		}
		return path, nil
	}
	if !strings.HasPrefix(digest, "sha256:") || len(digest) != len("sha256:")+64 {
		return "", manifest.ErrInvalidDigestFormat
	}
	path := filepath.Join(modelRoot, "blobs", strings.ReplaceAll(digest, ":", "-"))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("%w: ensure path elements are traversable", err)
	}
	return path, nil
}

func linkedGGUFConfigLayer(modelRoot string, f *ggml.GGML) (*manifest.Layer, error) {
	kv := f.KV()
	config := model.ConfigV2{
		ModelFormat:   f.Name(),
		ModelFamily:   kv.Architecture(),
		ModelFamilies: []string{kv.Architecture()},
		ModelType:     format.HumanNumber(kv.ParameterCount()),
		FileType:      kv.FileType().String(),
		Capabilities:  []string{string(model.CapabilityCompletion)},
		ContextLen:    int(kv.ContextLength()),
		EmbedLen:      int(kv.EmbeddingLength()),
		Architecture:  runtime.GOARCH,
		OS:            runtime.GOOS,
	}

	var b bytes.Buffer
	if err := json.NewEncoder(&b).Encode(config); err != nil {
		return nil, err
	}

	digest := fmt.Sprintf("sha256:%x", sha256.Sum256(b.Bytes()))
	blobPath, err := linkedBlobPath(modelRoot, digest)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(blobPath); errors.Is(err, os.ErrNotExist) {
		if err := os.WriteFile(blobPath, b.Bytes(), 0o644); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	layer := manifest.Layer{
		MediaType: "application/vnd.docker.container.image.v1+json",
		Digest:    digest,
		Size:      int64(b.Len()),
		Status:    fmt.Sprintf("using existing layer %s", digest),
	}
	return &layer, nil
}

func writeLinkedManifest(modelRoot string, name model.Name, config manifest.Layer, layers []manifest.Layer) error {
	if !name.IsValid() {
		return os.ErrNotExist
	}

	p := filepath.Join(modelRoot, "manifests", name.Filepath())
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}

	f, err := os.Create(p)
	if err != nil {
		return err
	}
	defer f.Close()

	m := manifest.Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.docker.distribution.manifest.v2+json",
		Config:        config,
		Layers:        layers,
	}

	return json.NewEncoder(f).Encode(m)
}

func digestForImportFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		return "", err
	}

	return fmt.Sprintf("sha256:%x", hash.Sum(nil)), nil
}

func (s *Server) setModelAlias(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Model       string `json:"model"`
		DisplayName string `json:"displayName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	modelName := strings.TrimSpace(req.Model)
	if modelName == "" {
		return fmt.Errorf("model is required")
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if err := s.Store.SetModelDisplayName(modelName, displayName); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) unlinkModel(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	modelName := strings.TrimSpace(req.Model)
	if modelName == "" {
		return fmt.Errorf("model is required")
	}

	metadata, err := s.Store.ModelMetadata()
	if err != nil {
		return err
	}
	var found store.ModelMetadata
	for _, item := range metadata {
		if item.ModelName == modelName {
			found = item
			break
		}
	}
	if found.Origin != "linked_gguf" {
		return fmt.Errorf("model is not a linked GGUF")
	}

	settings, err := s.Store.Settings()
	if err != nil {
		return fmt.Errorf("load model location: %w", err)
	}
	modelRoot := settings.Models
	if modelRoot == "" {
		modelRoot = envconfig.Models()
	}

	n := model.ParseName(modelName)
	if err := removeLinkedModel(modelRoot, n); err != nil {
		return err
	}
	if err := s.Store.DeleteModelMetadata(modelName); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func removeLinkedModel(modelRoot string, name model.Name) error {
	manifestPath := filepath.Join(modelRoot, "manifests", name.Filepath())
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("load linked manifest: %w", err)
	}

	var m manifest.Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("parse linked manifest: %w", err)
	}

	if err := os.Remove(manifestPath); err != nil {
		return fmt.Errorf("remove linked manifest: %w", err)
	}
	if err := manifest.PruneDirectory(filepath.Join(modelRoot, "manifests")); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("prune linked manifests: %w", err)
	}

	inUse, err := linkedManifestDigests(modelRoot)
	if err != nil {
		return fmt.Errorf("read linked manifests: %w", err)
	}
	for _, layer := range append(m.Layers, m.Config) {
		if layer.Digest == "" {
			continue
		}
		if _, ok := inUse[layer.Digest]; ok {
			continue
		}
		blobPath, err := linkedBlobPath(modelRoot, layer.Digest)
		if err != nil {
			return err
		}
		if err := os.Remove(blobPath); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove linked blob %s: %w", layer.Digest, err)
		}
	}

	return nil
}

func linkedManifestDigests(modelRoot string) (map[string]struct{}, error) {
	manifestsRoot := filepath.Join(modelRoot, "manifests")
	matches, err := filepath.Glob(filepath.Join(manifestsRoot, "*", "*", "*", "*"))
	if err != nil {
		return nil, err
	}

	inUse := map[string]struct{}{}
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			continue
		}
		data, err := os.ReadFile(match)
		if err != nil {
			return nil, err
		}
		var m manifest.Manifest
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, err
		}
		for _, layer := range append(m.Layers, m.Config) {
			if layer.Digest != "" {
				inUse[layer.Digest] = struct{}{}
			}
		}
	}

	return inUse, nil
}

func (s *Server) deleteManagedModel(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	modelName := strings.TrimSpace(req.Model)
	if modelName == "" {
		return fmt.Errorf("model is required")
	}

	metadata, err := s.Store.ModelMetadata()
	if err != nil {
		return err
	}
	for _, item := range metadata {
		if item.ModelName == modelName && item.Origin == "linked_gguf" {
			return fmt.Errorf("linked GGUF models must be unlinked, not deleted")
		}
	}

	if err := s.inferenceClient().Delete(r.Context(), &api.DeleteRequest{Model: modelName}); err != nil {
		return fmt.Errorf("delete model: %w", err)
	}
	if err := s.Store.DeleteModelMetadata(modelName); err != nil {
		return err
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

func (s *Server) cloudSetting(w http.ResponseWriter, r *http.Request) error {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	if err := s.Store.SetCloudEnabled(req.Enabled); err != nil {
		return fmt.Errorf("failed to persist cloud setting: %w", err)
	}

	s.Restart()

	return s.writeCloudStatus(w)
}

func (s *Server) getCloudSetting(w http.ResponseWriter, r *http.Request) error {
	return s.writeCloudStatus(w)
}

func (s *Server) writeCloudStatus(w http.ResponseWriter) error {
	disabled, source, err := s.Store.CloudStatus()
	if err != nil {
		return fmt.Errorf("failed to load cloud status: %w", err)
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(map[string]any{
		"disabled": disabled,
		"source":   source,
	})
}

func (s *Server) getCloudModels(w http.ResponseWriter, r *http.Request) error {
	w.Header().Set("Content-Type", "application/json")
	disabled, _, err := s.Store.CloudStatus()
	if err != nil {
		return fmt.Errorf("failed to load cloud status: %w", err)
	}
	if disabled {
		return json.NewEncoder(w).Encode(api.ListResponse{Models: []api.ListModelResponse{}})
	}

	list := s.ListCloudModels
	if list == nil {
		list = s.listCloudModels
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	models, err := list(ctx)
	if err != nil {
		var authErr api.AuthorizationError
		if errors.As(err, &authErr) && (authErr.StatusCode == http.StatusUnauthorized || authErr.StatusCode == http.StatusForbidden) {
			return json.NewEncoder(w).Encode(api.ListResponse{Models: []api.ListModelResponse{}})
		}
		return fmt.Errorf("failed to list cloud models: %w", err)
	}
	if models == nil {
		models = &api.ListResponse{Models: []api.ListModelResponse{}}
	}

	return json.NewEncoder(w).Encode(models)
}

func (s *Server) listCloudModels(ctx context.Context) (*api.ListResponse, error) {
	resp, err := s.doSelfSigned(ctx, http.MethodGet, "/api/tags")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, api.AuthorizationError{StatusCode: resp.StatusCode}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ollama.com/api/tags returned %s", resp.Status)
	}

	var models api.ListResponse
	if err := json.NewDecoder(resp.Body).Decode(&models); err != nil {
		return nil, fmt.Errorf("failed to parse cloud models: %w", err)
	}
	return &models, nil
}

func (s *Server) getInferenceCompute(w http.ResponseWriter, r *http.Request) error {
	ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
	defer cancel()
	info, err := server.GetInferenceInfo(ctx)
	if err != nil {
		s.log().Error("failed to get inference info", "error", err)
		return fmt.Errorf("failed to get inference info: %w", err)
	}

	inferenceComputes := make([]responses.InferenceCompute, len(info.Computes))
	for i, ic := range info.Computes {
		inferenceComputes[i] = responses.InferenceCompute{
			Library:     ic.Library,
			Variant:     ic.Variant,
			Compute:     ic.Compute,
			Driver:      ic.Driver,
			Name:        ic.Name,
			Description: ic.Description,
			VRAM:        ic.VRAM,
		}
	}

	response := responses.InferenceComputeResponse{
		InferenceComputes:    inferenceComputes,
		DefaultContextLength: info.DefaultContextLength,
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(response)
}

func (s *Server) modelUpstream(w http.ResponseWriter, r *http.Request) error {
	if r.Method != "POST" {
		return fmt.Errorf("method not allowed")
	}

	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return fmt.Errorf("invalid request body: %w", err)
	}

	if req.Model == "" {
		return fmt.Errorf("model is required")
	}

	digest, pushTime, err := s.checkModelUpstream(r.Context(), req.Model, 5*time.Second)
	if err != nil {
		s.log().Warn("failed to check upstream digest", "error", err, "model", req.Model)
		response := responses.ModelUpstreamResponse{
			Error: err.Error(),
		}
		w.Header().Set("Content-Type", "application/json")
		return json.NewEncoder(w).Encode(response)
	}

	n := model.ParseName(req.Model)
	stale := true
	if m, err := manifest.ParseNamedManifest(n); err == nil {
		if m.Digest() == digest {
			stale = false
		} else if pushTime > 0 && m.FileInfo().ModTime().Unix() >= pushTime {
			stale = false
		}
	}

	response := responses.ModelUpstreamResponse{
		Stale: stale,
	}

	w.Header().Set("Content-Type", "application/json")
	return json.NewEncoder(w).Encode(response)
}

func userAgent() string {
	buildinfo, _ := debug.ReadBuildInfo()

	version := buildinfo.Main.Version
	if version == "(devel)" {
		// When using `go run .` the version is "(devel)". This is seen
		// as an invalid version by ollama.com and so it defaults to
		// "needs upgrade" for some requests, such as pulls. These
		// checks can be skipped by using the special version "v0.0.0",
		// so we set it to that here.
		version = "v0.0.0"
	}

	return fmt.Sprintf("ollama/%s (%s %s) app/%s Go/%s",
		version,
		runtime.GOARCH,
		runtime.GOOS,
		version,
		runtime.Version(),
	)
}

// convertToOllamaTool converts a tool schema from our tools package format to Ollama API format
func convertToOllamaTool(toolSchema map[string]any) api.Tool {
	tool := api.Tool{
		Type: "function",
		Function: api.ToolFunction{
			Name:        getStringFromMap(toolSchema, "name", ""),
			Description: getStringFromMap(toolSchema, "description", ""),
		},
	}

	tool.Function.Parameters.Type = "object"
	tool.Function.Parameters.Required = []string{}
	tool.Function.Parameters.Properties = api.NewToolPropertiesMap()

	if schemaProps, ok := toolSchema["schema"].(map[string]any); ok {
		tool.Function.Parameters.Type = getStringFromMap(schemaProps, "type", "object")

		if props, ok := schemaProps["properties"].(map[string]any); ok {
			tool.Function.Parameters.Properties = api.NewToolPropertiesMap()

			for propName, propDef := range props {
				if propMap, ok := propDef.(map[string]any); ok {
					prop := api.ToolProperty{
						Type:        api.PropertyType{getStringFromMap(propMap, "type", "string")},
						Description: getStringFromMap(propMap, "description", ""),
					}
					tool.Function.Parameters.Properties.Set(propName, prop)
				}
			}
		}

		if required, ok := schemaProps["required"].([]string); ok {
			tool.Function.Parameters.Required = required
		} else if requiredAny, ok := schemaProps["required"].([]any); ok {
			required := make([]string, len(requiredAny))
			for i, r := range requiredAny {
				if s, ok := r.(string); ok {
					required[i] = s
				}
			}
			tool.Function.Parameters.Required = required
		}
	}

	return tool
}

// getStringFromMap safely gets a string from a map
func getStringFromMap(m map[string]any, key, defaultValue string) string {
	if val, ok := m[key].(string); ok {
		return val
	}
	return defaultValue
}

// isImageAttachment checks if a filename is an image file
func isImageAttachment(filename string) bool {
	ext := strings.ToLower(filename)
	return strings.HasSuffix(ext, ".png") || strings.HasSuffix(ext, ".jpg") || strings.HasSuffix(ext, ".jpeg") || strings.HasSuffix(ext, ".webp")
}

// ptr is a convenience function for &literal
func ptr[T any](v T) *T { return &v }

// Browser tools simulate a full browser environment, allowing for actions like searching, opening, and interacting with web pages (e.g., "browser_search", "browser_open", "browser_find"). Currently only gpt-oss models support browser tools.
func supportsBrowserTools(model string) bool {
	return strings.HasPrefix(strings.ToLower(model), "gpt-oss")
}

func (s *Server) chatToolRegistry() *tools.Registry {
	if s.ToolRegistry != nil {
		return s.ToolRegistry.Clone()
	}

	return tools.NewRegistry()
}

func requestToolRegistry(persistent *tools.Registry, hasToolsCapability, fileToolsEnabled bool) *tools.Registry {
	if hasToolsCapability && fileToolsEnabled && persistent != nil {
		return persistent.Clone()
	}

	return tools.NewRegistry()
}

// buildChatRequest converts store.Chat to api.ChatRequest
func (s *Server) buildChatRequest(chat *store.Chat, model string, think any, availableTools []map[string]any) (*api.ChatRequest, error) {
	var msgs []api.Message
	for _, m := range chat.Messages {
		// Skip empty messages if present
		if m.Content == "" && m.Thinking == "" && len(m.ToolCalls) == 0 && len(m.Attachments) == 0 {
			continue
		}

		apiMsg := api.Message{Role: m.Role, Thinking: m.Thinking}

		sb := strings.Builder{}
		sb.WriteString(m.Content)

		var images []api.ImageData
		if m.Role == "user" && len(m.Attachments) > 0 {
			for _, a := range m.Attachments {
				if isImageAttachment(a.Filename) {
					images = append(images, api.ImageData(a.Data))
				} else {
					content := convertBytesToText(a.Data, a.Filename)
					sb.WriteString(fmt.Sprintf("\n--- File: %s ---\n%s\n--- End of %s ---",
						a.Filename, content, a.Filename))
				}
			}
		}

		apiMsg.Content = sb.String()
		apiMsg.Images = images

		switch m.Role {
		case "assistant":
			if len(m.ToolCalls) > 0 {
				var toolCalls []api.ToolCall
				for _, tc := range m.ToolCalls {
					var args api.ToolCallFunctionArguments
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &args); err != nil {
						s.log().Error("failed to parse tool call arguments", "error", err, "function_name", tc.Function.Name, "arguments", tc.Function.Arguments)
						continue
					}

					toolCalls = append(toolCalls, api.ToolCall{
						Function: api.ToolCallFunction{
							Name:      tc.Function.Name,
							Arguments: args,
						},
					})
				}
				apiMsg.ToolCalls = toolCalls
			}
		case "tool":
			apiMsg.Role = "tool"
			apiMsg.Content = m.Content
			apiMsg.ToolName = m.ToolName
		case "user", "system":
			// User and system messages are handled normally
		default:
			// Log unknown roles but still include them
			s.log().Debug("unknown message role", "role", m.Role)
		}

		msgs = append(msgs, apiMsg)
	}

	var thinkValue *api.ThinkValue
	if think != nil {
		// Only set Think if it's actually requesting thinking
		if boolValue, ok := think.(bool); ok {
			if boolValue {
				thinkValue = &api.ThinkValue{Value: boolValue}
			}
		} else if stringValue, ok := think.(string); ok {
			if stringValue != "" && stringValue != "none" {
				thinkValue = &api.ThinkValue{Value: stringValue}
			}
		}
	}

	req := &api.ChatRequest{
		Model:    model,
		Messages: msgs,
		Stream:   ptr(true),
		Think:    thinkValue,
	}

	if len(availableTools) > 0 {
		tools := make(api.Tools, len(availableTools))
		for i, toolSchema := range availableTools {
			tools[i] = convertToOllamaTool(toolSchema)
		}
		req.Tools = tools
	}

	return req, nil
}

func countAPIMessageImages(messages []api.Message) int {
	total := 0
	for _, msg := range messages {
		total += len(msg.Images)
	}
	return total
}

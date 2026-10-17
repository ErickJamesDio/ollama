package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ollama/ollama/app/version"
)

type stdioClient struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser
	logger *slog.Logger

	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[int64]chan rpcEnvelope
	nextID  atomic.Int64
	closed  chan struct{}
	waitErr chan error
}

func newStdioClient(ctx context.Context, cfg Config, logger *slog.Logger) (*stdioClient, error) {
	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	cmd.Env = mergeEnv(os.Environ(), cfg.Env)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	client := &stdioClient{
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
		stderr:  stderr,
		logger:  logger,
		pending: make(map[int64]chan rpcEnvelope),
		closed:  make(chan struct{}),
		waitErr: make(chan error, 1),
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start process: %w", err)
	}
	if logger != nil {
		logger.Info("mcp process started", "pid", cmd.Process.Pid, "command", cfg.Command, "args", cfg.Args)
	}

	go client.readLoop()
	go client.logStderr()
	go client.waitLoop()

	return client, nil
}

func (c *stdioClient) Initialize(ctx context.Context) error {
	c.logger.Debug("mcp initialize request")
	var result initializeResult
	if err := c.request(ctx, "initialize", initializeParams{
		ProtocolVersion: protocolVersion,
		Capabilities:    map[string]any{},
		ClientInfo: clientInfo{
			Name:    "ollama-desktop",
			Version: version.Version,
		},
	}, &result); err != nil {
		return fmt.Errorf("initialize request: %w", err)
	}
	c.logger.Debug("mcp initialize response", "server", result.ServerInfo.Name, "version", result.ServerInfo.Version, "protocol", result.ProtocolVersion)

	c.logger.Debug("mcp initialized notification")
	if err := c.notify(ctx, "notifications/initialized", map[string]any{}); err != nil {
		return fmt.Errorf("initialized notification: %w", err)
	}
	c.logger.Debug("mcp initialized notification sent")

	return nil
}

func (c *stdioClient) ListTools(ctx context.Context) ([]toolDefinition, error) {
	var all []toolDefinition
	cursor := ""

	for {
		var result listToolsResult
		c.logger.Debug("mcp tools/list request", "cursor", cursor)
		if err := c.request(ctx, "tools/list", listToolsParams{Cursor: cursor}, &result); err != nil {
			return nil, fmt.Errorf("tools/list request: %w", err)
		}
		c.logger.Debug("mcp tools/list response", "tool_count", len(result.Tools), "next_cursor", result.NextCursor)

		all = append(all, result.Tools...)
		if result.NextCursor == "" {
			return all, nil
		}

		cursor = result.NextCursor
	}
}

func (c *stdioClient) CallTool(ctx context.Context, name string, arguments map[string]any) (callToolResult, error) {
	var result callToolResult
	c.logger.Debug("mcp tools/call request", "tool", name)
	err := c.request(ctx, "tools/call", callToolParams{
		Name:      name,
		Arguments: arguments,
	}, &result)
	if err == nil {
		c.logger.Debug("mcp tools/call response", "tool", name, "content_items", len(result.Content), "is_error", result.IsError)
	}
	return result, err
}

func (c *stdioClient) Close() error {
	_ = c.stdin.Close()

	select {
	case err := <-c.waitErr:
		if err != nil && !isProcessExit(err) {
			return err
		}
		return nil
	case <-time.After(2 * time.Second):
		if c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
		}
		err := <-c.waitErr
		if err != nil && !isProcessExit(err) {
			return err
		}
		return nil
	}
}

func (c *stdioClient) request(ctx context.Context, method string, params any, out any) error {
	id := c.nextID.Add(1)
	respCh := make(chan rpcEnvelope, 1)

	c.mu.Lock()
	c.pending[id] = respCh
	c.mu.Unlock()

	if err := c.sendRequest(ctx, id, method, params); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return err
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		c.logger.Warn("mcp request timed out", "method", method, "id", id, "error", ctx.Err())
		return ctx.Err()
	case <-c.closed:
		return errors.New("mcp process closed")
	case resp := <-respCh:
		if resp.Error != nil {
			return fmt.Errorf("%s failed: %s", method, resp.Error.Message)
		}
		if out == nil || len(resp.Result) == 0 {
			return nil
		}
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("decode %s response: %w", method, err)
		}
		return nil
	}
}

func (c *stdioClient) notify(ctx context.Context, method string, params any) error {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"method":  method,
		"params":  params,
	}
	return c.writeMessage(ctx, payload)
}

func (c *stdioClient) sendRequest(ctx context.Context, id int64, method string, params any) error {
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	}
	c.logger.Debug("mcp request send", "method", method, "id", id)
	return c.writeMessage(ctx, payload)
}

func (c *stdioClient) writeMessage(ctx context.Context, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return errors.New("mcp process closed")
	default:
	}

	body = append(body, '\n')
	if _, err := c.stdin.Write(body); err != nil {
		return fmt.Errorf("write body: %w", err)
	}

	return nil
}

func (c *stdioClient) readLoop() {
	reader := bufio.NewReader(c.stdout)
	for {
		msg, err := readEnvelope(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				c.logger.Debug("mcp stdout reader stopped", "error", err)
			}
			c.failPending(err)
			return
		}

		id, ok := parseEnvelopeID(msg.ID)
		if !ok {
			if msg.Method != "" {
				c.logger.Debug("ignoring mcp notification", "method", msg.Method)
			}
			continue
		}

		c.mu.Lock()
		respCh := c.pending[id]
		delete(c.pending, id)
		c.mu.Unlock()

		if respCh != nil {
			c.logger.Debug("mcp response received", "id", id, "method", msg.Method, "has_error", msg.Error != nil)
			respCh <- msg
		}
	}
}

func (c *stdioClient) waitLoop() {
	err := c.cmd.Wait()
	if err != nil && !isProcessExit(err) {
		c.logger.Warn("mcp process exited with error", "error", err)
	}
	c.waitErr <- err
	c.failPending(err)
}

func (c *stdioClient) failPending(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	select {
	case <-c.closed:
		return
	default:
		close(c.closed)
	}

	for id, respCh := range c.pending {
		respCh <- rpcEnvelope{Error: &rpcError{Message: fmt.Sprintf("process closed: %v", err)}}
		delete(c.pending, id)
	}
}

func (c *stdioClient) logStderr() {
	scanner := bufio.NewScanner(c.stderr)
	buffer := make([]byte, 0, 64*1024)
	scanner.Buffer(buffer, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		c.logger.Info("mcp stderr", "line", line)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		c.logger.Debug("mcp stderr reader stopped", "error", err)
	}
}

func readEnvelope(reader *bufio.Reader) (rpcEnvelope, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return rpcEnvelope{}, err
	}

	trimmed := strings.TrimRight(line, "\r\n")
	if strings.HasPrefix(strings.ToLower(trimmed), "content-length:") {
		return readContentLengthEnvelope(reader, trimmed)
	}

	return decodeEnvelope([]byte(trimmed))
}

func readContentLengthEnvelope(reader *bufio.Reader, firstHeader string) (rpcEnvelope, error) {
	contentLength := -1
	headers := []string{firstHeader}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return rpcEnvelope{}, err
		}

		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		headers = append(headers, line)
	}

	for _, line := range headers {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			return rpcEnvelope{}, fmt.Errorf("invalid header line %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(key), "Content-Length") {
			var err error
			contentLength, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return rpcEnvelope{}, fmt.Errorf("invalid content length: %w", err)
			}
		}
	}

	if contentLength < 0 {
		return rpcEnvelope{}, errors.New("missing content length")
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		return rpcEnvelope{}, err
	}

	return decodeEnvelope(body)
}

func decodeEnvelope(body []byte) (rpcEnvelope, error) {
	var msg rpcEnvelope
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&msg); err != nil {
		return rpcEnvelope{}, fmt.Errorf("decode envelope: %w", err)
	}
	return msg, nil
}

func parseEnvelopeID(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}

	var id int64
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, true
	}

	return 0, false
}

func mergeEnv(base []string, overrides map[string]string) []string {
	if len(overrides) == 0 {
		return base
	}

	merged := make(map[string]string, len(base)+len(overrides))
	for _, item := range base {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			merged[key] = value
		}
	}
	for key, value := range overrides {
		merged[key] = value
	}

	result := make([]string, 0, len(merged))
	for key, value := range merged {
		result = append(result, key+"="+value)
	}
	return result
}

func isProcessExit(err error) bool {
	if err == nil {
		return false
	}

	var exitErr *exec.ExitError
	return errors.As(err, &exitErr)
}

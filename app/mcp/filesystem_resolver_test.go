package mcp

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestFilesystemServerArgs(t *testing.T) {
	cfg := Config{
		Command: "cmd",
		Args:    []string{"/c", "npx", "-y", filesystemServerPackage, `D:\`, `C:\Users\test\.lmstudio`},
	}
	want := []string{`D:\`, `C:\Users\test\.lmstudio`}
	got, ok := filesystemServerArgs(cfg)
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("filesystemServerArgs() = %#v, %v; want %#v, true", got, ok, want)
	}
}

func TestNewestFilesystemEntrypointUsesInstalledPackage(t *testing.T) {
	root := filepath.Join(t.TempDir(), "_npx")
	packageDir := filepath.Join(root, "cache-key", "node_modules", "@modelcontextprotocol", "server-filesystem")
	entrypoint := filepath.Join(packageDir, "dist", "index.js")
	if err := os.MkdirAll(filepath.Dir(entrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `{"name":"@modelcontextprotocol/server-filesystem","bin":{"mcp-server-filesystem":"dist/index.js"}}`
	if err := os.WriteFile(filepath.Join(packageDir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entrypoint, []byte("// test"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := newestFilesystemEntrypoint([]string{root}); got != entrypoint {
		t.Fatalf("newestFilesystemEntrypoint() = %q, want %q", got, entrypoint)
	}
}

func TestMCPStartupTimeout(t *testing.T) {
	t.Setenv("OLLAMA_MCP_STARTUP_TIMEOUT", "27s")
	if got := mcpStartupTimeout(); got != 27*time.Second {
		t.Fatalf("mcpStartupTimeout() = %s, want 27s", got)
	}

	t.Setenv("OLLAMA_MCP_STARTUP_TIMEOUT", "invalid")
	if got := mcpStartupTimeout(); got != defaultStartupTimeout {
		t.Fatalf("invalid timeout = %s, want %s", got, defaultStartupTimeout)
	}
}

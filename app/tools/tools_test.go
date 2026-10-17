//go:build windows || darwin

package tools

import (
	"context"
	"errors"
	"testing"
)

type testTool struct {
	name string
}

func (t testTool) Name() string { return t.name }

func (t testTool) Description() string { return "test" }

func (t testTool) Schema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func (t testTool) Execute(context.Context, map[string]any) (any, string, error) {
	return nil, "", nil
}

func (t testTool) Prompt() string { return "" }

func TestRegistryReplaceSource(t *testing.T) {
	registry := NewRegistry()
	registry.Register(testTool{name: "builtin.tool"})
	registry.ReplaceSource("plugin:mcp:one", []Tool{
		testTool{name: "mcp.one.echo"},
	})
	registry.ReplaceSource("plugin:mcp:two", []Tool{
		testTool{name: "mcp.two.echo"},
	})

	if _, ok := registry.Get("mcp.one.echo"); !ok {
		t.Fatal("expected source one tool to be registered")
	}
	if _, ok := registry.Get("mcp.two.echo"); !ok {
		t.Fatal("expected source two tool to be registered")
	}

	registry.ReplaceSource("plugin:mcp:one", []Tool{
		testTool{name: "mcp.one.updated"},
	})

	if _, ok := registry.Get("mcp.one.echo"); ok {
		t.Fatal("expected stale tool from source one to be removed")
	}
	if _, ok := registry.Get("mcp.one.updated"); !ok {
		t.Fatal("expected replacement tool from source one to be registered")
	}
	if _, ok := registry.Get("builtin.tool"); !ok {
		t.Fatal("expected builtin tool to remain registered")
	}
	if _, ok := registry.Get("mcp.two.echo"); !ok {
		t.Fatal("expected source two tool to remain registered")
	}

	registry.RemoveSource("plugin:mcp:two")
	if _, ok := registry.Get("mcp.two.echo"); ok {
		t.Fatal("expected source two tool to be removed")
	}
}

type partialFailureTool struct{}

func (partialFailureTool) Name() string           { return "partial.failure" }
func (partialFailureTool) Description() string    { return "test" }
func (partialFailureTool) Schema() map[string]any { return map[string]any{"type": "object"} }
func (partialFailureTool) Prompt() string         { return "" }
func (partialFailureTool) Execute(context.Context, map[string]any) (any, string, error) {
	return map[string]any{"exitCode": 1}, "stderr from tool", errors.New("tool failed")
}

func TestRegistryPreservesPartialResultOnError(t *testing.T) {
	registry := NewRegistry()
	registry.Register(partialFailureTool{})

	result, text, err := registry.Execute(t.Context(), "partial.failure", nil)
	if err == nil {
		t.Fatal("Execute() error = nil, want failure")
	}
	if text != "stderr from tool" {
		t.Fatalf("Execute() text = %q", text)
	}
	if result == nil {
		t.Fatal("Execute() discarded partial result")
	}
}

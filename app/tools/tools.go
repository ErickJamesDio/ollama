//go:build windows || darwin

package tools

import (
	"context"
	"encoding/json"
	"fmt"
)

// Tool defines the interface that all tools must implement
type Tool interface {
	// Name returns the unique identifier for the tool
	Name() string

	// Description returns a human-readable description of what the tool does
	Description() string

	// Schema returns the JSON schema for the tool's parameters
	Schema() map[string]any

	// Execute runs the tool with the given arguments and returns result to store in db, and a string result for the model
	Execute(ctx context.Context, args map[string]any) (any, string, error)

	// Prompt returns a prompt for the tool
	Prompt() string
}

// Registry manages the available tools and their execution
type Registry struct {
	tools       map[string]Tool
	toolSources map[string]string
	sourceTools map[string][]string
	workingDir  string // Working directory for all tool operations
}

// NewRegistry creates a new tool registry with no tools
func NewRegistry() *Registry {
	return &Registry{
		tools:       make(map[string]Tool),
		toolSources: make(map[string]string),
		sourceTools: make(map[string][]string),
	}
}

// Clone creates a shallow copy of the registry so request-scoped tools can be
// added without mutating the shared persistent registry.
func (r *Registry) Clone() *Registry {
	clone := NewRegistry()
	if r == nil {
		return clone
	}

	clone.workingDir = r.workingDir
	for name, tool := range r.tools {
		clone.tools[name] = tool
	}
	for name, source := range r.toolSources {
		clone.toolSources[name] = source
	}
	for source, names := range r.sourceTools {
		clone.sourceTools[source] = append([]string(nil), names...)
	}

	return clone
}

// Register adds a tool to the registry
func (r *Registry) Register(tool Tool) {
	r.RegisterFromSource("", tool)
}

// RegisterFromSource adds a tool to the registry and associates it with a
// stable source identifier so it can be replaced or removed later.
func (r *Registry) RegisterFromSource(source string, tool Tool) {
	if r.tools == nil {
		r.tools = make(map[string]Tool)
	}
	if r.toolSources == nil {
		r.toolSources = make(map[string]string)
	}
	if r.sourceTools == nil {
		r.sourceTools = make(map[string][]string)
	}

	name := tool.Name()
	if previousSource, ok := r.toolSources[name]; ok {
		r.removeToolFromSource(previousSource, name)
	}

	r.tools[name] = tool
	r.toolSources[name] = source
	r.sourceTools[source] = appendUnique(r.sourceTools[source], name)
}

// ReplaceSource replaces all tools owned by source with the provided set.
func (r *Registry) ReplaceSource(source string, tools []Tool) {
	r.RemoveSource(source)
	for _, tool := range tools {
		r.RegisterFromSource(source, tool)
	}
}

// RemoveSource removes all tools that were previously registered for source.
func (r *Registry) RemoveSource(source string) {
	names := append([]string(nil), r.sourceTools[source]...)
	for _, name := range names {
		if currentSource, ok := r.toolSources[name]; ok && currentSource == source {
			delete(r.tools, name)
			delete(r.toolSources, name)
		}
	}
	delete(r.sourceTools, source)
}

// Get retrieves a tool by name
func (r *Registry) Get(name string) (Tool, bool) {
	tool, exists := r.tools[name]
	return tool, exists
}

// List returns all available tools
func (r *Registry) List() []Tool {
	tools := make([]Tool, 0, len(r.tools))
	for _, tool := range r.tools {
		tools = append(tools, tool)
	}
	return tools
}

// SetWorkingDir sets the working directory for all tool operations
func (r *Registry) SetWorkingDir(dir string) {
	r.workingDir = dir
}

// Execute runs a tool with the given name and arguments
func (r *Registry) Execute(ctx context.Context, name string, args map[string]any) (any, string, error) {
	tool, ok := r.tools[name]
	if !ok {
		return nil, "", fmt.Errorf("unknown tool: %s", name)
	}

	result, text, err := tool.Execute(ctx, args)
	return result, text, err
}

// ToolCall represents a request to execute a tool
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolFunction `json:"function"`
}

// ToolFunction represents the function call details
type ToolFunction struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult represents the result of a tool execution
type ToolResult struct {
	ToolCallID string `json:"tool_call_id"`
	Content    any    `json:"content"`
	Error      string `json:"error,omitempty"`
}

// ToolSchemas returns all tools as schema maps suitable for API calls
func (r *Registry) AvailableTools() []map[string]any {
	schemas := make([]map[string]any, 0, len(r.tools))
	for _, tool := range r.tools {
		schema := map[string]any{
			"name":        tool.Name(),
			"description": tool.Description(),
			"schema":      tool.Schema(),
		}
		schemas = append(schemas, schema)
	}
	return schemas
}

// ToolNames returns a list of all tool names
func (r *Registry) ToolNames() []string {
	names := make([]string, 0, len(r.tools))
	for name := range r.tools {
		names = append(names, name)
	}
	return names
}

func (r *Registry) removeToolFromSource(source, name string) {
	names := r.sourceTools[source]
	filtered := names[:0]
	for _, candidate := range names {
		if candidate != name {
			filtered = append(filtered, candidate)
		}
	}
	if len(filtered) == 0 {
		delete(r.sourceTools, source)
		return
	}
	r.sourceTools[source] = filtered
}

func appendUnique(items []string, item string) []string {
	for _, existing := range items {
		if existing == item {
			return items
		}
	}
	return append(items, item)
}

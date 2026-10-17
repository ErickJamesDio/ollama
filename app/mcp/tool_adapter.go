package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ollama/ollama/app/store"
)

type ToolAdapter struct {
	manager     *Manager
	plugin      store.Plugin
	publicName  string
	remoteName  string
	description string
	inputSchema map[string]any
}

func NewToolAdapter(manager *Manager, plugin store.Plugin, tool toolDefinition) *ToolAdapter {
	schema := tool.InputSchema
	if schema == nil {
		schema = map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		}
	}

	return &ToolAdapter{
		manager:     manager,
		plugin:      plugin,
		publicName:  namespacedToolName(plugin.Name, tool.Name),
		remoteName:  tool.Name,
		description: tool.Description,
		inputSchema: schema,
	}
}

func (t *ToolAdapter) Name() string {
	return t.publicName
}

func (t *ToolAdapter) Description() string {
	if strings.TrimSpace(t.description) != "" {
		return t.description
	}
	return fmt.Sprintf("MCP tool %s from plugin %s.", t.remoteName, t.plugin.Name)
}

func (t *ToolAdapter) Schema() map[string]any {
	return t.inputSchema
}

func (t *ToolAdapter) Execute(ctx context.Context, args map[string]any) (any, string, error) {
	result, err := t.manager.CallTool(ctx, t.plugin.ID, t.remoteName, args)
	text := renderToolResult(result)
	payload, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return result, text, err
	}
	raw := json.RawMessage(payload)
	return &raw, text, err
}

func (t *ToolAdapter) Prompt() string {
	return ""
}

func namespacedToolName(pluginName, toolName string) string {
	return "mcp." + sanitizeNameComponent(pluginName) + "." + sanitizeNameComponent(toolName)
}

func sanitizeNameComponent(input string) string {
	input = strings.TrimSpace(strings.ToLower(input))
	if input == "" {
		return "tool"
	}

	var builder strings.Builder
	lastUnderscore := false
	for _, r := range input {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			builder.WriteRune(r)
			lastUnderscore = false
		case r == '-' || r == '_' || r == '.':
			builder.WriteRune(r)
			lastUnderscore = false
		default:
			if !lastUnderscore {
				builder.WriteRune('_')
				lastUnderscore = true
			}
		}
	}

	result := strings.Trim(builder.String(), "._-")
	if result == "" {
		return "tool"
	}
	return result
}

func renderToolResult(result callToolResult) string {
	var parts []string
	for _, item := range result.Content {
		if item.Type == "text" && strings.TrimSpace(item.Text) != "" {
			parts = append(parts, item.Text)
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n")
	}

	if result.StructuredContent != nil {
		if body, err := json.Marshal(result.StructuredContent); err == nil {
			return string(body)
		}
	}

	if body, err := json.Marshal(result); err == nil {
		return string(body)
	}

	return ""
}

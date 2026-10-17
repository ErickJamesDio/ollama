package mcp

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ollama/ollama/app/store"
)

type Config struct {
	Command   string
	Args      []string
	Env       map[string]string
	Transport string
}

func ParsePluginConfig(plugin store.Plugin) (Config, error) {
	var raw struct {
		Command   string            `json:"command"`
		Args      []string          `json:"args"`
		Env       map[string]string `json:"env"`
		Transport string            `json:"transport"`
	}

	if err := json.Unmarshal(plugin.Config, &raw); err != nil {
		return Config{}, fmt.Errorf("parse plugin %q config: %w", plugin.Name, err)
	}

	cfg := Config{
		Command:   strings.TrimSpace(raw.Command),
		Args:      append([]string(nil), raw.Args...),
		Env:       raw.Env,
		Transport: strings.TrimSpace(raw.Transport),
	}

	if cfg.Transport == "" {
		cfg.Transport = "stdio"
	}
	if cfg.Transport != "stdio" {
		return Config{}, fmt.Errorf("plugin %q uses unsupported transport %q", plugin.Name, cfg.Transport)
	}
	if cfg.Command == "" {
		return Config{}, fmt.Errorf("plugin %q must define a command for stdio transport", plugin.Name)
	}
	if cfg.Env == nil {
		cfg.Env = map[string]string{}
	}

	return cfg, nil
}

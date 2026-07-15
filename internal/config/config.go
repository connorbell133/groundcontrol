// Package config defines the config file schema and loads it. The live copy is
// owned by the API server (the only mutator, via PUT /config).
package config

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/connorbell133/groundcontrol/internal/events"
)

type Config struct {
	Port       int                    `json:"port"`
	Host       string                 `json:"host,omitempty"`
	Roots      []string               `json:"roots"`
	ShowHidden bool                   `json:"showHidden"`
	Webhooks   []events.WebhookConfig `json:"webhooks,omitempty"`
	Jobs       *JobsConfig            `json:"jobs,omitempty"`
	AuthToken  string                 `json:"authToken,omitempty"`
	Tokens     []TokenConfig          `json:"tokens,omitempty"`
}

type JobsConfig struct {
	Concurrency int `json:"concurrency,omitempty"`
	TimeoutMs   int `json:"timeoutMs,omitempty"`
}

// Scoped tokens for automations: read (browse/inspect), launch (spawn/kill
// sessions and jobs), admin (config writes, worktree force-removal). The
// legacy authToken keeps full scope — an n8n token gets read,launch and can
// never widen roots.
type TokenConfig struct {
	Name   string   `json:"name"`
	Token  string   `json:"token"`
	Scopes []string `json:"scopes"`
}

// Load reads and parses the config file at path.
func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("failed to read %s: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return Config{}, fmt.Errorf("failed to parse %s: %w", path, err)
	}
	var rawKeys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawKeys); err == nil {
		if _, ok := rawKeys["ntfy"]; ok {
			fmt.Fprintln(os.Stderr, "config.ntfy is no longer used — notifications are generic webhooks now; see the webhooks key in README")
		}
	}
	return cfg, nil
}

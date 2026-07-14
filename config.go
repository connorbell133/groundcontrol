package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
)

type Config struct {
	Port       int             `json:"port"`
	Host       string          `json:"host,omitempty"`
	Roots      []string        `json:"roots"`
	ShowHidden bool            `json:"showHidden"`
	Webhooks   []WebhookConfig `json:"webhooks,omitempty"`
	Jobs       *JobsConfig     `json:"jobs,omitempty"`
	AuthToken  string          `json:"authToken,omitempty"`
	Tokens     []TokenConfig   `json:"tokens,omitempty"`
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

// loadConfig reads and parses the config file at path.
func loadConfig(path string) (Config, error) {
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

// applyAndPersistConfig re-applies the live parts of the config, then persists
// to disk; the caller (PUT /config) holds configMu across the mutation and
// this call.
func (a *app) applyAndPersistConfig() {
	a.configureBrowser(a.cfg.Roots, a.cfg.ShowHidden)
	a.configureWebhooks(a.cfg.Webhooks)
	out, err := json.MarshalIndent(&a.cfg, "", "  ")
	if err != nil {
		log.Printf("failed to serialize config: %v", err)
		return
	}
	if err := os.WriteFile(a.configPath, append(out, '\n'), 0o644); err != nil {
		log.Printf("failed to write %s: %v", a.configPath, err)
	}
}

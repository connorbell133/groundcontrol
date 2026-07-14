package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

const version = "0.4.0"

// configMu guards the shared *Config. The PUT /config handler in api.go
// mutates the config and calls persistConfig while holding this lock; readers
// (auth middleware, GET /config) take it briefly to snapshot.
var configMu sync.Mutex

//go:embed all:public
var publicEmbed embed.FS

func main() {
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatalf("cannot determine working directory: %v", err)
	}
	configPath := filepath.Join(cwd, "config.json")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		log.Fatalf("failed to read %s: %v", configPath, err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Fatalf("failed to parse %s: %v", configPath, err)
	}
	var rawKeys map[string]json.RawMessage
	if err := json.Unmarshal(raw, &rawKeys); err == nil {
		if _, ok := rawKeys["ntfy"]; ok {
			fmt.Fprintln(os.Stderr, "config.ntfy is no longer used — notifications are generic webhooks now; see the webhooks key in README")
		}
	}

	configureBrowser(cfg.Roots, cfg.ShowHidden)
	configureWebhooks(cfg.Webhooks)
	configureJobs(cfg.Jobs)
	sweepOrphanWorktrees()

	// re-apply the live parts of the config, then persist to disk; the caller
	// (PUT /config) holds configMu across the mutation and this call
	applyAndPersistConfig := func() {
		configureBrowser(cfg.Roots, cfg.ShowHidden)
		configureWebhooks(cfg.Webhooks)
		out, err := json.MarshalIndent(&cfg, "", "  ")
		if err != nil {
			log.Printf("failed to serialize config: %v", err)
			return
		}
		if err := os.WriteFile(configPath, append(out, '\n'), 0o644); err != nil {
			log.Printf("failed to write %s: %v", configPath, err)
		}
	}

	mux := http.NewServeMux()

	// unauthenticated by design (uptime probes)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ready := 0
		for _, s := range listSessions() {
			if s.State == "ready" {
				ready++
			}
		}
		writeJSON(w, 200, struct {
			OK       bool   `json:"ok"`
			Version  string `json:"version"`
			Sessions int    `json:"sessions"`
		}{true, version, ready})
	})

	api := createAPI(&cfg, applyAndPersistConfig)
	mux.Handle("/api/v1/", http.StripPrefix("/api/v1", api)) // canonical, documented in docs/api.md
	mux.Handle("/api/", http.StripPrefix("/api", api))       // deprecated alias for pinned clients — kept for one release

	mime.AddExtensionType(".webmanifest", "application/manifest+json")
	pub, err := fs.Sub(publicEmbed, "public")
	if err != nil {
		log.Fatalf("failed to open embedded public dir: %v", err)
	}
	// no directory listings: FileServer would render an index for folders like
	// /icons/ — Hono's serveStatic 404ed, so we do too
	fileServer := http.FileServerFS(pub)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p != "" && p != "." {
			if info, err := fs.Stat(pub, p); err == nil && info.IsDir() {
				if _, err := fs.Stat(pub, path.Join(p, "index.html")); err != nil {
					http.NotFound(w, r)
					return
				}
			}
		}
		fileServer.ServeHTTP(w, r)
	}))

	host := cfg.Host
	if host == "" {
		host = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(cfg.Port)))
	if err != nil {
		log.Fatalf("failed to listen on %s:%d: %v", host, cfg.Port, err)
	}
	port := cfg.Port
	if addr, ok := ln.Addr().(*net.TCPAddr); ok {
		port = addr.Port
	}
	fmt.Printf("groundcontrol listening on http://%s:%d\n", host, port)
	log.Fatal(http.Serve(ln, mux))
}

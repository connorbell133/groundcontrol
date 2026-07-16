package main

import (
	"context"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	groundcontrol "github.com/connorbell133/groundcontrol"
	"github.com/connorbell133/groundcontrol/internal/api"
	"github.com/connorbell133/groundcontrol/internal/browse"
	"github.com/connorbell133/groundcontrol/internal/config"
	"github.com/connorbell133/groundcontrol/internal/events"
	"github.com/connorbell133/groundcontrol/internal/jobs"
	"github.com/connorbell133/groundcontrol/internal/journal"
	"github.com/connorbell133/groundcontrol/internal/sessions"
	"github.com/connorbell133/groundcontrol/internal/util"
	"github.com/connorbell133/groundcontrol/internal/workspace"
)

// release builds override this via -ldflags "-X main.version=..."
var version = "0.6.0"

func main() {
	configPath := flag.String("config", "config.json", "path to the config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("groundcontrol " + version)
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	// one journal, one bus, one manager per domain — wired here, nowhere else
	jnl := journal.New(filepath.Join(util.MustCwd(), "data"))
	bus := events.NewBus(jnl)
	browser := browse.New()
	ws := workspace.New(filepath.Join(util.MustHome(), ".groundcontrol", "worktrees"), jnl)
	sessionMgr := sessions.NewManager(jnl, bus, ws, browser)
	jobMgr := jobs.NewManager(jnl, bus, ws)

	browser.Configure(cfg.Roots, cfg.ShowHidden)
	bus.ConfigureWebhooks(cfg.Webhooks)
	if cfg.Jobs != nil {
		jobMgr.Configure(cfg.Jobs.Concurrency, cfg.Jobs.TimeoutMs)
	}
	// The sweep treats every on-disk worktree as a dead runner's leftover — true
	// only for the sole holder of the runner lock. A second instance (a dev copy
	// run from a checkout shares ~/.groundcontrol/worktrees) must not sweep the
	// first one's live worktrees out from under its sessions.
	if ws.AcquireRunnerLock() {
		ws.SweepOrphans()
		// same single-runner gate: a crashed runner's injected settings files
		// (journaled as settingsInjected on session.start) are leftovers only
		// when no other instance could still own live sessions in them
		sessionMgr.SweepSettingsLeftovers()
	} else {
		log.Printf("another groundcontrol instance is running — skipping the orphan-worktree sweep")
		jnl.Append(map[string]any{"event": "worktree.sweep-skipped", "reason": "another runner holds the lock"})
	}

	mux := http.NewServeMux()

	// unauthenticated by design (uptime probes)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ready := 0
		for _, s := range sessionMgr.List() {
			if s.State == sessions.StateReady {
				ready++
			}
		}
		api.WriteJSON(w, 200, struct {
			OK       bool   `json:"ok"`
			Version  string `json:"version"`
			Sessions int    `json:"sessions"`
		}{true, version, ready})
	})

	// unauthenticated by design, same as /healthz: it's a reference page, not
	// a route that reads or changes state, and the Scalar CDN page has no way
	// to send an auth header when it fetches the spec.
	mux.HandleFunc("GET /docs", api.ServeScalarDocs)
	mux.HandleFunc("GET /openapi.yaml", api.ServeOpenAPISpec)

	srvAPI := api.NewServer(*configPath, cfg, browser, bus, ws, sessionMgr, jobMgr).Handler()
	mux.Handle("/api/v1/", http.StripPrefix("/api/v1", srvAPI)) // canonical, documented in docs/api.md
	mux.Handle("/api/", http.StripPrefix("/api", srvAPI))       // deprecated alias for pinned clients — kept for one release

	mime.AddExtensionType(".webmanifest", "application/manifest+json")
	pub, err := fs.Sub(groundcontrol.PublicFS, "public")
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

	// Spawned agents learn where the runner lives via GROUNDCONTROL_URL, so a
	// stuck job or session can call back and launch a rescue session in its
	// own working directory (the stuck-agent handoff, docs/api.md). Agents run
	// on this box, so loopback is always reachable even when the server binds
	// a broader interface like 0.0.0.0.
	advertiseHost := host
	if advertiseHost == "0.0.0.0" || advertiseHost == "::" {
		advertiseHost = "127.0.0.1"
	}
	advertisedURL := fmt.Sprintf("http://%s/api/v1", net.JoinHostPort(advertiseHost, strconv.Itoa(port)))
	sessionMgr.SetAdvertisedURL(advertisedURL)
	jobMgr.SetAdvertisedURL(advertisedURL)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// the registry poller (claude agents --json enrichment) rides the same
	// signal context as the server: armed here, it runs only while sessions
	// are live and dies with the process
	sessionMgr.StartRegistryLoop(ctx)

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		// No ReadTimeout/WriteTimeout: /api/v1/events is a long-lived SSE
		// response, and WriteTimeout would kill the stream mid-flight.

		// Tie request contexts to the signal context so the SSE handler's
		// r.Context().Done() fires on shutdown — otherwise srv.Shutdown would
		// hang forever waiting on never-ending SSE responses.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-serveErr:
		log.Fatalf("server error: %v", err)
	case <-ctx.Done():
	}
	stop() // restore default signal handling so a second ^C force-kills

	log.Println("signal received, shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown timed out, closing: %v", err)
		srv.Close()
	}

	// Kill live work explicitly: sessions hold PTYs and worktrees whose
	// cleanup runs on their exit path, so we drive them to exit rather than
	// abandoning them to die with the process.
	for _, s := range sessionMgr.List() {
		if s.State == sessions.StateStarting || s.State == sessions.StateReady {
			sessionMgr.Kill(s.ID, "shutdown")
		}
	}
	for _, j := range jobMgr.List() {
		if j.State == jobs.StateQueued || j.State == jobs.StateRunning {
			jobMgr.Cancel(j.ID, "shutdown")
		}
	}

	// Wait for the readLoop/finishJob paths to finish worktree cleanup before
	// exiting; bounded so a wedged PTY can't hold shutdown hostage.
	deadline := time.Now().Add(10 * time.Second)
	for {
		live := 0
		for _, s := range sessionMgr.List() {
			if s.State != sessions.StateExited && s.State != sessions.StateError {
				live++
			}
		}
		for _, j := range jobMgr.List() {
			if j.State == jobs.StateQueued || j.State == jobs.StateRunning {
				live++
			}
		}
		if live == 0 {
			break
		}
		if time.Now().After(deadline) {
			log.Printf("shutdown wait deadline passed with %d sessions/jobs still live", live)
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.Println("shutdown complete")
}

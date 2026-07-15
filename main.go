package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// release builds override this via -ldflags "-X main.version=..."
var version = "0.4.0"

//go:embed all:public
var publicEmbed embed.FS

func main() {
	configPath := flag.String("config", "config.json", "path to the config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("groundcontrol " + version)
		return
	}

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}

	a := newApp(*configPath, cfg)
	a.configureBrowser(cfg.Roots, cfg.ShowHidden)
	a.configureWebhooks(cfg.Webhooks)
	a.configureJobs(cfg.Jobs)
	a.sweepOrphanWorktrees()

	mux := http.NewServeMux()

	// unauthenticated by design (uptime probes)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ready := 0
		for _, s := range a.listSessions() {
			if s.State == stateReady {
				ready++
			}
		}
		writeJSON(w, 200, struct {
			OK       bool   `json:"ok"`
			Version  string `json:"version"`
			Sessions int    `json:"sessions"`
		}{true, version, ready})
	})

	// unauthenticated by design, same as /healthz: it's a reference page, not
	// a route that reads or changes state, and the Scalar CDN page has no way
	// to send an auth header when it fetches the spec.
	mux.HandleFunc("GET /docs", serveScalarDocs)
	mux.HandleFunc("GET /openapi.yaml", serveOpenAPISpec)

	api := a.createAPI()
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
	for _, s := range a.listSessions() {
		if s.State == "starting" || s.State == "ready" {
			a.killSession(s.ID, "shutdown")
		}
	}
	for _, j := range a.listJobs() {
		if j.State == "queued" || j.State == "running" {
			a.cancelJob(j.ID, "shutdown")
		}
	}

	// Wait for the readLoop/finishJob paths to finish worktree cleanup before
	// exiting; bounded so a wedged PTY can't hold shutdown hostage.
	deadline := time.Now().Add(10 * time.Second)
	for {
		live := 0
		for _, s := range a.listSessions() {
			if s.State != "exited" && s.State != "error" {
				live++
			}
		}
		for _, j := range a.listJobs() {
			if j.State == "queued" || j.State == "running" {
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

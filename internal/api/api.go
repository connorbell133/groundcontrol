// Package api serves the HTTP API: routing, auth (tokens + scopes), request
// validation, and the JSON/SSE wire formats. It owns the live config — the
// auth middleware reads it and PUT /config is its only mutator.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	qrcode "github.com/skip2/go-qrcode"

	"github.com/connorbell133/groundcontrol/internal/browse"
	"github.com/connorbell133/groundcontrol/internal/config"
	"github.com/connorbell133/groundcontrol/internal/events"
	"github.com/connorbell133/groundcontrol/internal/jobs"
	"github.com/connorbell133/groundcontrol/internal/sessions"
	"github.com/connorbell133/groundcontrol/internal/util"
	"github.com/connorbell133/groundcontrol/internal/workspace"
)

// Stable machine-readable codes — clients key off these, never off message text.
// Documented in docs/api.md; adding a code is fine, renaming one is a breaking change.
// Codes in use: unauthorized, insufficient_scope, invalid_json, missing_param,
// invalid_param, invalid_path, invalid_config, outside_roots, not_found, not_ready,
// ready_timeout, launch_failed, session_live, job_live, worktree_error, not_implemented,
// branch_held, branch_not_merged.

// scope names one capability a token can grant; the wire format stays the
// plain string ("read" etc.) in config files and error messages.
type scope string

const (
	scopeRead   scope = "read"   // browse, inspect, watch events
	scopeLaunch scope = "launch" // spawn and kill sessions and jobs
	scopeAdmin  scope = "admin"  // config writes, worktree force-removal
)

var allScopes = []scope{scopeRead, scopeLaunch, scopeAdmin}

type apiActor struct {
	name   string
	scopes map[scope]bool
}

type actorCtxKey struct{}

func actorOf(r *http.Request) apiActor {
	if a, ok := r.Context().Value(actorCtxKey{}).(apiActor); ok {
		return a
	}
	return apiActor{name: "anonymous", scopes: map[scope]bool{}}
}

func scopeSet(scopes []scope) map[scope]bool {
	set := make(map[scope]bool, len(scopes))
	for _, s := range scopes {
		set[s] = true
	}
	return set
}

func apiErr(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, message}})
}

// WriteJSON marshals v and writes it with the given status; exported so main
// can serve /healthz in the same wire format.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(data)
}

func writeText(w http.ResponseWriter, status int, s string) {
	w.Header().Set("Content-Type", "text/plain; charset=UTF-8")
	w.WriteHeader(status)
	io.WriteString(w, s)
}

var httpURL = regexp.MustCompile(`^https?://`)

// nonNil guarantees JSON `[]` (never `null`) for list responses.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// decodeBody unmarshals the request body into dst and writes the 400 itself
// on failure: malformed JSON (or a non-object body) is invalid_json, a field
// of the wrong JSON type is invalid_param. Unknown fields are ignored.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		apiErr(w, 400, "invalid_json", "request body must be valid JSON")
		return false
	}
	if err := json.Unmarshal(data, dst); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Field != "" {
			apiErr(w, 400, "invalid_param", fmt.Sprintf("%s must be a JSON %s", typeErr.Field, jsonTypeName(typeErr.Type)))
		} else {
			apiErr(w, 400, "invalid_json", "request body must be valid JSON")
		}
		return false
	}
	return true
}

// jsonTypeName names the Go target type in the client's vocabulary — error
// messages should say "array", not "[]string".
func jsonTypeName(t reflect.Type) string {
	switch t.Kind() {
	case reflect.Bool:
		return "boolean"
	case reflect.Float32, reflect.Float64,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "number"
	case reflect.String:
		return "string"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	default:
		return t.String()
	}
}

// known token, missing scope → 403 (vs 401 for no/unknown token)
func need(s scope, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		act := actorOf(r)
		if !act.scopes[s] {
			apiErr(w, 403, "insufficient_scope", fmt.Sprintf("token \"%s\" lacks the %s scope", act.name, s))
			return
		}
		h(w, r)
	}
}

// qrSVG hand-builds the SVG the npm qrcode package produced: 1-module quiet
// zone, dark modules #1c1a14 on #fffdf6, one square path segment per module.
func qrSVG(content string) (string, error) {
	q, err := qrcode.New(content, qrcode.Medium)
	if err != nil {
		return "", err
	}
	q.DisableBorder = true
	bm := q.Bitmap()
	total := len(bm) + 2 // 1-module margin on each side
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, total, total)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fffdf6"/>`, total, total)
	b.WriteString(`<path fill="#1c1a14" d="`)
	for y, row := range bm {
		for x, dark := range row {
			if dark {
				fmt.Fprintf(&b, "M%d %dh1v1h-1z", x+1, y+1)
			}
		}
	}
	b.WriteString(`"/></svg>`)
	return b.String(), nil
}

// tokenEqual compares in constant time so response latency can't confirm a
// partially-guessed token. Length still leaks (ConstantTimeCompare bails on
// mismatched lengths) — fine, tokens are random, not structured.
func tokenEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// Server wires the domain managers to HTTP. It also owns the live config:
// the auth middleware snapshots it per request, and PUT /config mutates it
// under configMu, re-applies the live parts, and persists.
type Server struct {
	configMu   sync.Mutex
	cfg        config.Config
	configPath string

	browser  *browse.Browser
	bus      *events.Bus
	ws       *workspace.Manager
	sessions *sessions.Manager
	jobs     *jobs.Manager
}

func NewServer(configPath string, cfg config.Config, browser *browse.Browser, bus *events.Bus, ws *workspace.Manager, sm *sessions.Manager, jm *jobs.Manager) *Server {
	return &Server{
		cfg:        cfg,
		configPath: configPath,
		browser:    browser,
		bus:        bus,
		ws:         ws,
		sessions:   sm,
		jobs:       jm,
	}
}

// applyAndPersistConfigLocked re-applies the live parts of the config, then
// persists to disk; the caller (PUT /config) holds configMu across the
// mutation and this call.
func (s *Server) applyAndPersistConfigLocked() {
	s.browser.Configure(s.cfg.Roots, s.cfg.ShowHidden)
	s.bus.ConfigureWebhooks(s.cfg.Webhooks)
	out, err := json.MarshalIndent(&s.cfg, "", "  ")
	if err != nil {
		log.Printf("failed to serialize config: %v", err)
		return
	}
	if err := os.WriteFile(s.configPath, append(out, '\n'), 0o644); err != nil {
		log.Printf("failed to write %s: %v", s.configPath, err)
	}
}

// Every route requires a token when any is configured; the resolved actor
// (name + scopes) travels on the request context and into the journal.
// ?token= is accepted too because <img> tags (the QR) and EventSource can't send headers.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.configMu.Lock()
		authToken := s.cfg.AuthToken
		tokens := make([]config.TokenConfig, len(s.cfg.Tokens))
		copy(tokens, s.cfg.Tokens)
		s.configMu.Unlock()

		var act apiActor
		if authToken == "" && len(tokens) == 0 {
			act = apiActor{name: "anonymous", scopes: scopeSet(allScopes)}
		} else {
			header := r.Header.Get("Authorization")
			var token string
			if strings.HasPrefix(header, "Bearer ") {
				token = header[len("Bearer "):]
			} else {
				token = r.URL.Query().Get("token")
			}
			switch {
			case token != "" && authToken != "" && tokenEqual(token, authToken):
				act = apiActor{name: "owner", scopes: scopeSet(allScopes)}
			default:
				var entry *config.TokenConfig
				if token != "" {
					// compare against every entry — no early exit on match, so
					// timing doesn't narrow down which entry (if any) it was
					for i := range tokens {
						if tokenEqual(token, tokens[i].Token) && entry == nil {
							entry = &tokens[i]
						}
					}
				}
				if entry == nil {
					apiErr(w, 401, "unauthorized", "missing or invalid bearer token")
					return
				}
				// unknown scope strings are filtered out rather than rejected
				scopes := map[scope]bool{}
				for _, sc := range entry.Scopes {
					for _, known := range allScopes {
						if scope(sc) == known {
							scopes[known] = true
						}
					}
				}
				act = apiActor{name: entry.Name, scopes: scopes}
			}
		}
		ctx := context.WithValue(r.Context(), actorCtxKey{}, act)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

type createSessionRequest struct {
	Folder         string `json:"folder"`
	Name           string `json:"name"`
	SpawnMode      string `json:"spawnMode"`
	Branch         string `json:"branch"`
	PermissionMode string `json:"permissionMode"`
	Capacity       int    `json:"capacity"`   // 0/absent → CLI default; normalized in Create, never rejected
	PresetName     string `json:"presetName"` // journaled launch fact, so recents/relaunch can re-resolve it
	// pointers so "sent but empty/zero" still fails validation while "absent"
	// falls back cleanly
	CallbackURL *string  `json:"callbackUrl"`
	TimeoutMs   *float64 `json:"timeoutMs"` // only meaningful with ?wait=ready
}

// permissionModes is the CLI's six documented modes (2.1.210). Empty means
// "let Create default it"; anything else fails fast here as invalid_param
// instead of reaching claude.
var permissionModes = map[string]bool{
	"default":           true,
	"acceptEdits":       true,
	"plan":              true,
	"auto":              true,
	"dontAsk":           true,
	"bypassPermissions": true,
}

type createJobRequest struct {
	Folder         string   `json:"folder"`
	Prompt         string   `json:"prompt"`
	SpawnMode      string   `json:"spawnMode"`
	Branch         string   `json:"branch"`
	PermissionMode string   `json:"permissionMode"`
	CallbackURL    *string  `json:"callbackUrl"`
	TimeoutMs      *float64 `json:"timeoutMs"`
	// the launch console's wired-for-v1 toggles: accepted, never silently ignored
	Isolation bool `json:"isolation"`
	Docker    bool `json:"docker"`
}

type putConfigRequest struct {
	// pointers keep present-vs-absent apart — PUT /config is a partial update
	Roots      *[]string               `json:"roots"`
	ShowHidden *bool                   `json:"showHidden"`
	Webhooks   *[]events.WebhookConfig `json:"webhooks"`
	Presets    *[]config.Preset        `json:"presets"`
}

// Handler returns the API handler, mounted by main at /api/v1 (canonical) and
// /api (deprecated alias for one release).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /roots", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, struct {
			Roots []browse.FolderEntry `json:"roots"`
		}{nonNil(s.browser.ListRoots())})
	}))

	mux.HandleFunc("GET /browse", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			apiErr(w, 400, "missing_param", "path required")
			return
		}
		result, err := s.browser.Browse(path)
		if err != nil {
			apiErr(w, 400, "invalid_path", err.Error())
			return
		}
		WriteJSON(w, 200, result)
	}))

	mux.HandleFunc("GET /branches", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			apiErr(w, 400, "missing_param", "path required")
			return
		}
		list, err := s.browser.BranchList(path)
		if err != nil {
			apiErr(w, 400, "invalid_path", err.Error())
			return
		}
		WriteJSON(w, 200, struct {
			Branches []string `json:"branches"`
		}{nonNil(list)})
	}))

	mux.HandleFunc("GET /config", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		s.configMu.Lock()
		roots := append([]string(nil), s.cfg.Roots...)
		showHidden := s.cfg.ShowHidden
		webhooks := append([]events.WebhookConfig(nil), s.cfg.Webhooks...)
		presets := append([]config.Preset(nil), s.cfg.Presets...)
		s.configMu.Unlock()
		if webhooks == nil {
			webhooks = []events.WebhookConfig{}
		}
		WriteJSON(w, 200, struct {
			Roots      []string               `json:"roots"`
			ShowHidden bool                   `json:"showHidden"`
			Webhooks   []events.WebhookConfig `json:"webhooks"`
			Presets    []config.Preset        `json:"presets"`
		}{nonNil(roots), showHidden, webhooks, nonNil(presets)})
	}))

	mux.HandleFunc("PUT /config", need(scopeAdmin, func(w http.ResponseWriter, r *http.Request) {
		var req putConfigRequest
		if !decodeBody(w, r, &req) {
			return
		}

		// validate everything before touching cfg so a bad field can't leave
		// a half-applied config in memory
		if req.Roots != nil {
			if len(*req.Roots) == 0 {
				apiErr(w, 400, "invalid_config", "roots must be a non-empty list")
				return
			}
			for _, root := range *req.Roots {
				if !strings.HasPrefix(root, "/") {
					apiErr(w, 400, "invalid_config", fmt.Sprintf("root must be an absolute path: %s", root))
					return
				}
				if st, err := os.Stat(root); err != nil || !st.IsDir() {
					apiErr(w, 400, "invalid_config", fmt.Sprintf("not a directory: %s", root))
					return
				}
			}
		}
		if req.Webhooks != nil {
			for _, hook := range *req.Webhooks {
				if !httpURL.MatchString(hook.URL) {
					apiErr(w, 400, "invalid_config", fmt.Sprintf("webhook url must be http(s): %s", hook.URL))
					return
				}
				if hook.Events == nil {
					continue
				}
				for _, e := range *hook.Events {
					if strings.TrimSpace(e) == "" {
						apiErr(w, 400, "invalid_config", "webhook events must be a list of non-empty strings")
						return
					}
				}
			}
		}
		if req.Presets != nil {
			seen := map[string]bool{}
			for _, p := range *req.Presets {
				if strings.TrimSpace(p.Name) == "" {
					apiErr(w, 400, "invalid_config", "preset name must be non-empty")
					return
				}
				if seen[p.Name] {
					apiErr(w, 400, "invalid_config", fmt.Sprintf("duplicate preset name: %s", p.Name))
					return
				}
				seen[p.Name] = true
				switch p.PermissionMode {
				case "", "default", "acceptEdits", "plan", "auto", "dontAsk", "bypassPermissions":
				default:
					apiErr(w, 400, "invalid_config", fmt.Sprintf("preset %s: unknown permission mode: %s", p.Name, p.PermissionMode))
					return
				}
				switch p.SpawnMode {
				case "", "same-dir", "worktree":
				default:
					apiErr(w, 400, "invalid_config", fmt.Sprintf("preset %s: unknown spawn mode: %s", p.Name, p.SpawnMode))
					return
				}
				// 0 means unset — the launch default applies
				if p.Capacity < 0 || p.Capacity > 256 {
					apiErr(w, 400, "invalid_config", fmt.Sprintf("preset %s: capacity must be 1..256: %d", p.Name, p.Capacity))
					return
				}
				if p.SettingsJSON != "" {
					if len(p.SettingsJSON) > 64*1024 {
						apiErr(w, 400, "invalid_config", fmt.Sprintf("preset %s: settings JSON exceeds 64 KB", p.Name))
						return
					}
					// a nil map after a clean parse means the literal null
					var settings map[string]json.RawMessage
					if err := json.Unmarshal([]byte(p.SettingsJSON), &settings); err != nil || settings == nil {
						apiErr(w, 400, "invalid_config", fmt.Sprintf("preset %s: settings JSON must be a JSON object", p.Name))
						return
					}
					if _, ok := settings["hooks"]; ok {
						apiErr(w, 400, "invalid_config", fmt.Sprintf("preset %s: settings must not carry a hooks key — hooks run shell commands and are a separately gated feature", p.Name))
						return
					}
				}
			}
		}

		s.configMu.Lock()
		defer s.configMu.Unlock()
		if req.Roots != nil {
			s.cfg.Roots = *req.Roots
		}
		if req.ShowHidden != nil {
			s.cfg.ShowHidden = *req.ShowHidden
		}
		if req.Webhooks != nil {
			s.cfg.Webhooks = *req.Webhooks
		}
		if req.Presets != nil {
			s.cfg.Presets = *req.Presets
		}
		s.applyAndPersistConfigLocked()
		WriteJSON(w, 200, struct {
			OK bool `json:"ok"`
		}{true})
	}))

	mux.HandleFunc("GET /worktrees", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, struct {
			Worktrees []workspace.Kept `json:"worktrees"`
		}{nonNil(s.ws.ListKept(s.sessions.LiveWorktreePaths()))})
	}))

	mux.HandleFunc("DELETE /worktrees", need(scopeAdmin, func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			apiErr(w, 400, "missing_param", "path required")
			return
		}
		if err := s.ws.ForceRemove(path); err != nil {
			apiErr(w, 400, "worktree_error", err.Error())
			return
		}
		WriteJSON(w, 200, struct {
			OK bool `json:"ok"`
		}{true})
	}))

	mux.HandleFunc("GET /orbit", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, struct {
			Orbit []sessions.OrbitBranch `json:"orbit"`
		}{nonNil(s.sessions.ListOrbit())})
	}))

	mux.HandleFunc("DELETE /orbit", need(scopeAdmin, func(w http.ResponseWriter, r *http.Request) {
		repo := r.URL.Query().Get("repo")
		branch := r.URL.Query().Get("branch")
		if repo == "" || branch == "" {
			apiErr(w, 400, "missing_param", "repo and branch required")
			return
		}
		// gc/ gate first — before any filesystem or git work, so a crafted
		// name (a flag, a user branch) is refused outright
		if !strings.HasPrefix(branch, "gc/") {
			apiErr(w, 400, "invalid_param", "only gc/* session branches can be swept")
			return
		}
		if !s.browser.WithinRoots(repo) || !util.PathExists(repo) {
			apiErr(w, 400, "invalid_path", "repo outside configured roots, or gone")
			return
		}
		err := s.sessions.SweepOrbitBranch(repo, branch)
		switch {
		case err == nil:
			WriteJSON(w, 200, struct {
				OK bool `json:"ok"`
			}{true})
		case errors.Is(err, sessions.ErrOrbitRepo):
			apiErr(w, 400, "invalid_path", err.Error())
		case errors.Is(err, sessions.ErrOrbitNotFound):
			apiErr(w, 404, "not_found", err.Error())
		case errors.Is(err, sessions.ErrOrbitHeld):
			apiErr(w, 409, "branch_held", err.Error())
		default:
			// git refused the safe delete: unmerged commits — there is no
			// force path; merge the work or clean its worktree first
			apiErr(w, 409, "branch_not_merged", err.Error())
		}
	}))

	mux.HandleFunc("GET /sessions", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		// a reader is watching: flip the registry poller to its fast tier
		s.sessions.MarkObserved()
		WriteJSON(w, 200, struct {
			Sessions []sessions.Session       `json:"sessions"`
			Lost     []sessions.LostSession   `json:"lost"`
			Landed   []sessions.LandedSession `json:"landed"`
		}{nonNil(s.sessions.List()), nonNil(s.sessions.ListLost()), nonNil(s.sessions.ListLanded())})
	}))

	mux.HandleFunc("GET /journal/recent", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		limit := 5
		if q := r.URL.Query().Get("limit"); q != "" {
			n, err := strconv.Atoi(q)
			if err != nil {
				apiErr(w, 400, "invalid_param", "limit must be an integer")
				return
			}
			limit = min(20, max(1, n))
		}
		WriteJSON(w, 200, struct {
			Recent []sessions.RecentLaunch `json:"recent"`
		}{nonNil(s.sessions.RecentLaunches(limit))})
	}))

	// Live lifecycle stream (SSE): session.start/ready/exit/kill as they happen.
	// No replay — reconnecting clients should re-list first; the journal is the history.
	mux.HandleFunc("GET /events", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(200)

		// buffered channel keeps the emit goroutine from ever blocking on a
		// slow client; a full buffer drops frames (clients re-list on reconnect)
		ch := make(chan events.LifecycleEvent, 64)
		unsub := s.bus.OnEvent(func(e events.LifecycleEvent) {
			select {
			case ch <- e:
			default:
			}
		})
		defer unsub()

		hello, _ := json.Marshal(struct {
			At string `json:"at"`
		}{util.NowISO()})
		fmt.Fprintf(w, "event: hello\ndata: %s\n\n", hello)
		fl.Flush()

		ticker := time.NewTicker(25 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case e := <-ch:
				data, err := json.Marshal(e)
				if err != nil {
					continue
				}
				fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Event, data)
				fl.Flush()
			case <-ticker.C:
				fmt.Fprint(w, "event: ping\ndata: \n\n")
				fl.Flush()
			}
		}
	}))

	mux.HandleFunc("POST /sessions", need(scopeLaunch, func(w http.ResponseWriter, r *http.Request) {
		var req createSessionRequest
		if !decodeBody(w, r, &req) {
			return
		}
		if req.Folder == "" {
			apiErr(w, 400, "missing_param", "folder required")
			return
		}
		if !s.browser.WithinRoots(req.Folder) {
			apiErr(w, 400, "outside_roots", "folder outside configured roots")
			return
		}
		if req.PermissionMode != "" && !permissionModes[req.PermissionMode] {
			apiErr(w, 400, "invalid_param", "permissionMode must be one of default, acceptEdits, plan, auto, dontAsk, bypassPermissions")
			return
		}
		var callbackURL string
		if req.CallbackURL != nil {
			if !httpURL.MatchString(*req.CallbackURL) {
				apiErr(w, 400, "invalid_param", "callbackUrl must be an http(s) URL")
				return
			}
			callbackURL = *req.CallbackURL
		}

		// resolve the named preset: it fills only the launch options the
		// request left empty (request wins), and supplies the settings JSON
		// for injection. A name that no longer resolves is not an error —
		// relaunching a deleted preset must keep working — the launch just
		// proceeds without injection and carries the skip reason (R8).
		var settingsJSON, settingsSkipReason string
		if req.PresetName != "" {
			s.configMu.Lock()
			var preset *config.Preset
			for i := range s.cfg.Presets {
				if s.cfg.Presets[i].Name == req.PresetName {
					preset = &s.cfg.Presets[i]
					break
				}
			}
			if preset != nil {
				if req.PermissionMode == "" {
					req.PermissionMode = preset.PermissionMode
				}
				if req.SpawnMode == "" {
					req.SpawnMode = preset.SpawnMode
				}
				if req.Capacity == 0 {
					req.Capacity = preset.Capacity
				}
				settingsJSON = preset.SettingsJSON
			} else {
				settingsSkipReason = "preset no longer exists"
			}
			s.configMu.Unlock()
		}

		session, err := s.sessions.Create(sessions.CreateOpts{
			Folder:             req.Folder,
			Name:               req.Name,
			SpawnMode:          req.SpawnMode,
			Branch:             req.Branch,
			PermissionMode:     req.PermissionMode,
			Capacity:           req.Capacity,
			PresetName:         req.PresetName,
			SettingsJSON:       settingsJSON,
			SettingsSkipReason: settingsSkipReason,
			CallbackURL:        callbackURL,
			Actor:              actorOf(r).name,
		})
		if err != nil {
			apiErr(w, 409, "launch_failed", err.Error())
			return
		}

		if r.URL.Query().Get("wait") != "ready" {
			WriteJSON(w, 201, session)
			return
		}

		// ?wait=ready: one round-trip to a pairing URL for scripts and automations
		timeoutMs := 60000.0
		if req.TimeoutMs != nil {
			timeoutMs = min(300000, max(1000, *req.TimeoutMs))
		}
		outcome := s.sessions.WaitForReady(session.ID, time.Duration(timeoutMs*float64(time.Millisecond)))
		latest := session
		if snap := s.sessions.Get(session.ID); snap != nil {
			latest = *snap
		}
		switch outcome {
		case "ready":
			WriteJSON(w, 201, latest)
		case "dead":
			msg := fmt.Sprintf("session %s exited before pairing", session.ID)
			if latest.ExitCode != nil {
				msg += fmt.Sprintf(" (code %d)", *latest.ExitCode)
			}
			if latest.LastLine != nil && *latest.LastLine != "" {
				msg += " — " + *latest.LastLine
			}
			apiErr(w, 409, "launch_failed", msg)
		default:
			apiErr(w, 504, "ready_timeout", fmt.Sprintf("session %s not ready after %sms — still starting; poll GET /sessions/%s",
				session.ID, strconv.FormatFloat(timeoutMs, 'f', -1, 64), session.ID))
		}
	}))

	/* ---------- headless jobs: claude -p, no phone in the loop ---------- */

	mux.HandleFunc("POST /jobs", need(scopeLaunch, func(w http.ResponseWriter, r *http.Request) {
		var req createJobRequest
		if !decodeBody(w, r, &req) {
			return
		}
		if req.Folder == "" {
			apiErr(w, 400, "missing_param", "folder required")
			return
		}
		if strings.TrimSpace(req.Prompt) == "" {
			apiErr(w, 400, "missing_param", "prompt required")
			return
		}
		if !s.browser.WithinRoots(req.Folder) {
			apiErr(w, 400, "outside_roots", "folder outside configured roots")
			return
		}
		var callbackURL string
		if req.CallbackURL != nil {
			if !httpURL.MatchString(*req.CallbackURL) {
				apiErr(w, 400, "invalid_param", "callbackUrl must be an http(s) URL")
				return
			}
			callbackURL = *req.CallbackURL
		}
		// honest about the launch console's wired-for-v1 toggle: accepted, never ignored
		if req.Isolation || req.Docker {
			apiErr(w, 501, "not_implemented", "Docker isolation is not implemented yet — jobs run as the runner's user")
			return
		}
		// absent → -1 so the jobs manager applies its default; an explicit 0 clamps to the 1s floor
		timeoutMs := -1
		if req.TimeoutMs != nil {
			timeoutMs = int(*req.TimeoutMs)
		}
		job, err := s.jobs.Create(jobs.CreateOpts{
			Folder:         req.Folder,
			Prompt:         req.Prompt,
			SpawnMode:      req.SpawnMode,
			Branch:         req.Branch,
			PermissionMode: req.PermissionMode,
			TimeoutMs:      timeoutMs,
			CallbackURL:    callbackURL,
			Actor:          actorOf(r).name,
		})
		if err != nil {
			apiErr(w, 409, "launch_failed", err.Error())
			return
		}
		WriteJSON(w, 202, job)
	}))

	mux.HandleFunc("GET /jobs", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		WriteJSON(w, 200, struct {
			Jobs []jobs.Job `json:"jobs"`
		}{nonNil(s.jobs.List())})
	}))

	mux.HandleFunc("GET /jobs/{id}", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		job := s.jobs.Get(r.PathValue("id"))
		if job == nil {
			apiErr(w, 404, "not_found", "no such job")
			return
		}
		WriteJSON(w, 200, job)
	}))

	mux.HandleFunc("GET /jobs/{id}/log", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		log := s.jobs.GetLog(r.PathValue("id"))
		if log == nil {
			apiErr(w, 404, "not_found", "no such job")
			return
		}
		writeText(w, 200, *log)
	}))

	mux.HandleFunc("DELETE /jobs/{id}", need(scopeLaunch, func(w http.ResponseWriter, r *http.Request) {
		job := s.jobs.Cancel(r.PathValue("id"), actorOf(r).name)
		if job == nil {
			apiErr(w, 404, "not_found", "no such job")
			return
		}
		WriteJSON(w, 200, job)
	}))

	mux.HandleFunc("DELETE /jobs/{id}/record", need(scopeLaunch, func(w http.ResponseWriter, r *http.Request) {
		removed, err := s.jobs.Remove(r.PathValue("id"))
		if err != nil {
			apiErr(w, 409, "job_live", err.Error())
			return
		}
		if !removed {
			apiErr(w, 404, "not_found", "no such job")
			return
		}
		WriteJSON(w, 200, struct {
			OK bool `json:"ok"`
		}{true})
	}))

	mux.HandleFunc("GET /sessions/{id}", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		session := s.sessions.Get(r.PathValue("id"))
		if session == nil {
			apiErr(w, 404, "not_found", "no such session")
			return
		}
		WriteJSON(w, 200, session)
	}))

	mux.HandleFunc("GET /sessions/{id}/qr", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		session := s.sessions.Get(r.PathValue("id"))
		if session == nil {
			apiErr(w, 404, "not_found", "no such session")
			return
		}
		if session.PairingURL == nil || *session.PairingURL == "" {
			apiErr(w, 409, "not_ready", "no pairing url yet")
			return
		}
		svg, err := qrSVG(*session.PairingURL)
		if err != nil {
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(200)
		io.WriteString(w, svg)
	}))

	mux.HandleFunc("GET /sessions/{id}/log", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		log := s.sessions.GetLog(r.PathValue("id"))
		if log == nil {
			apiErr(w, 404, "not_found", "no such session")
			return
		}
		writeText(w, 200, *log)
	}))

	// The actual conversations (user prompts + assistant replies), read from the
	// JSONL transcripts Claude Code writes for the session's launch directory.
	mux.HandleFunc("GET /sessions/{id}/transcript", need(scopeRead, func(w http.ResponseWriter, r *http.Request) {
		transcripts, found := s.sessions.GetTranscripts(r.PathValue("id"))
		if !found {
			apiErr(w, 404, "not_found", "no such session")
			return
		}
		WriteJSON(w, 200, struct {
			Transcripts []sessions.Transcript `json:"transcripts"`
		}{nonNil(transcripts)})
	}))

	mux.HandleFunc("DELETE /sessions/{id}", need(scopeLaunch, func(w http.ResponseWriter, r *http.Request) {
		session := s.sessions.Kill(r.PathValue("id"), actorOf(r).name)
		if session == nil {
			apiErr(w, 404, "not_found", "no such session")
			return
		}
		WriteJSON(w, 200, session)
	}))

	mux.HandleFunc("DELETE /sessions/{id}/record", need(scopeLaunch, func(w http.ResponseWriter, r *http.Request) {
		removed, err := s.sessions.Remove(r.PathValue("id"))
		if err != nil {
			apiErr(w, 409, "session_live", err.Error())
			return
		}
		if !removed {
			apiErr(w, 404, "not_found", "no such session")
			return
		}
		WriteJSON(w, 200, struct {
			OK bool `json:"ok"`
		}{true})
	}))

	return s.authMiddleware(mux)
}

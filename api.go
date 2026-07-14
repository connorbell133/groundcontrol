package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

// Stable machine-readable codes — clients key off these, never off message text.
// Documented in docs/api.md; adding a code is fine, renaming one is a breaking change.
// Codes in use: unauthorized, insufficient_scope, invalid_json, missing_param,
// invalid_param, invalid_path, invalid_config, outside_roots, not_found, not_ready,
// ready_timeout, launch_failed, session_live, job_live, worktree_error, not_implemented.

var allScopes = []string{"read", "launch", "admin"}

type apiActor struct {
	name   string
	scopes map[string]bool
}

type actorCtxKey struct{}

func actorOf(r *http.Request) apiActor {
	if a, ok := r.Context().Value(actorCtxKey{}).(apiActor); ok {
		return a
	}
	return apiActor{name: "anonymous", scopes: map[string]bool{}}
}

func scopeSet(scopes []string) map[string]bool {
	set := make(map[string]bool, len(scopes))
	for _, s := range scopes {
		set[s] = true
	}
	return set
}

func apiErr(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, message}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
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

// jsTruthy mirrors JS truthiness for a decoded JSON value: null/false/0/NaN/""
// are falsy, everything else (including empty arrays and objects) is truthy.
func jsTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && !math.IsNaN(x)
	case string:
		return x != ""
	default:
		return true
	}
}

// jsNumberOr mirrors `Number(v) || fallback`: NaN and 0 both yield the fallback.
func jsNumberOr(v any, fallback float64) float64 {
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case bool:
		if x {
			n = 1
		}
	case string:
		s := strings.TrimSpace(x)
		if s == "" {
			n = 0
		} else if f, err := strconv.ParseFloat(s, 64); err == nil {
			n = f
		} else {
			return fallback // NaN
		}
	default: // null/undefined/objects → NaN or 0; either way falsy
		return fallback
	}
	if n == 0 || math.IsNaN(n) {
		return fallback
	}
	return n
}

// jsDisplay renders a decoded JSON value the way a JS template literal would —
// used only inside error messages.
func jsDisplay(v any) string {
	switch x := v.(type) {
	case nil:
		// JSON null interpolates as "null" in TS template literals (undefined
		// can never arrive through JSON)
		return "null"
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

func strField(v any) string {
	s, _ := v.(string)
	return s
}

// nonNil guarantees JSON `[]` (never `null`) for list responses.
func nonNil[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}

// readJSONBody mirrors `await c.req.json().catch(() => null)` followed by the
// `!body` gate: a parse failure or a falsy top-level value means invalid_json.
// A truthy non-object parses fine in JS but every property read is undefined,
// so it comes back as an empty map here.
func readJSONBody(r *http.Request) (map[string]any, bool) {
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, false
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, false
	}
	if !jsTruthy(v) {
		return nil, false
	}
	m, ok := v.(map[string]any)
	if !ok {
		m = map[string]any{}
	}
	return m, true
}

// block until the session pairs, dies, or the deadline passes — 300ms poll is
// plenty against a multi-second provision and immune to event races
func waitForReady(id string, timeout time.Duration) string {
	deadline := time.Now().Add(timeout)
	for {
		s := getSession(id)
		if s == nil || s.State == "exited" || s.State == "error" {
			return "dead"
		}
		if s.State == "ready" {
			return "ready"
		}
		if !time.Now().Before(deadline) {
			return "timeout"
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// known token, missing scope → 403 (vs 401 for no/unknown token)
func need(scope string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		act := actorOf(r)
		if !act.scopes[scope] {
			apiErr(w, 403, "insufficient_scope", fmt.Sprintf("token \"%s\" lacks the %s scope", act.name, scope))
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

// Every route requires a token when any is configured; the resolved actor
// (name + scopes) travels on the request context and into the journal.
// ?token= is accepted too because <img> tags (the QR) and EventSource can't send headers.
func authMiddleware(cfg *Config, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		configMu.Lock()
		authToken := cfg.AuthToken
		tokens := make([]TokenConfig, len(cfg.Tokens))
		copy(tokens, cfg.Tokens)
		configMu.Unlock()

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
			case token != "" && authToken != "" && token == authToken:
				act = apiActor{name: "owner", scopes: scopeSet(allScopes)}
			default:
				var entry *TokenConfig
				if token != "" {
					for i := range tokens {
						if tokens[i].Token == token {
							entry = &tokens[i]
							break
						}
					}
				}
				if entry == nil {
					apiErr(w, 401, "unauthorized", "missing or invalid bearer token")
					return
				}
				// unknown scope strings are filtered out rather than rejected
				scopes := map[string]bool{}
				for _, s := range entry.Scopes {
					for _, known := range allScopes {
						if s == known {
							scopes[s] = true
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

// One sub-handler, mounted at /api/v1 (canonical) and /api (deprecated alias for one release).
func createAPI(cfg *Config, persistConfig func()) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /roots", need("read", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, struct {
			Roots []FolderEntry `json:"roots"`
		}{nonNil(listRoots())})
	}))

	mux.HandleFunc("GET /browse", need("read", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			apiErr(w, 400, "missing_param", "path required")
			return
		}
		result, err := browse(path)
		if err != nil {
			apiErr(w, 400, "invalid_path", err.Error())
			return
		}
		writeJSON(w, 200, result)
	}))

	mux.HandleFunc("GET /branches", need("read", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			apiErr(w, 400, "missing_param", "path required")
			return
		}
		list, err := branchList(path)
		if err != nil {
			apiErr(w, 400, "invalid_path", err.Error())
			return
		}
		writeJSON(w, 200, struct {
			Branches []string `json:"branches"`
		}{nonNil(list)})
	}))

	mux.HandleFunc("GET /config", need("read", func(w http.ResponseWriter, r *http.Request) {
		configMu.Lock()
		roots := append([]string(nil), cfg.Roots...)
		showHidden := cfg.ShowHidden
		webhooks := append([]WebhookConfig(nil), cfg.Webhooks...)
		configMu.Unlock()
		if webhooks == nil {
			webhooks = []WebhookConfig{}
		}
		writeJSON(w, 200, struct {
			Roots      []string        `json:"roots"`
			ShowHidden bool            `json:"showHidden"`
			Webhooks   []WebhookConfig `json:"webhooks"`
		}{nonNil(roots), showHidden, webhooks})
	}))

	mux.HandleFunc("PUT /config", need("admin", func(w http.ResponseWriter, r *http.Request) {
		body, ok := readJSONBody(r)
		if !ok {
			apiErr(w, 400, "invalid_json", "request body must be valid JSON")
			return
		}

		configMu.Lock()
		defer configMu.Unlock()

		if rootsV, present := body["roots"]; present {
			arr, isArr := rootsV.([]any)
			if !isArr || len(arr) == 0 {
				apiErr(w, 400, "invalid_config", "roots must be a non-empty list")
				return
			}
			roots := make([]string, 0, len(arr))
			for _, rv := range arr {
				s, isStr := rv.(string)
				if !isStr || !strings.HasPrefix(s, "/") {
					apiErr(w, 400, "invalid_config", fmt.Sprintf("root must be an absolute path: %s", jsDisplay(rv)))
					return
				}
				if st, err := os.Stat(s); err != nil || !st.IsDir() {
					apiErr(w, 400, "invalid_config", fmt.Sprintf("not a directory: %s", s))
					return
				}
				roots = append(roots, s)
			}
			cfg.Roots = roots
		}
		if shV, present := body["showHidden"]; present {
			cfg.ShowHidden = jsTruthy(shV)
		}
		if whV, present := body["webhooks"]; present {
			arr, isArr := whV.([]any)
			if !isArr {
				apiErr(w, 400, "invalid_config", "webhooks must be a list")
				return
			}
			hooks := make([]WebhookConfig, 0, len(arr))
			for _, wv := range arr {
				wm, _ := wv.(map[string]any)
				var urlV any
				if wm != nil {
					urlV = wm["url"]
				}
				u, isStr := urlV.(string)
				if !isStr || !httpURL.MatchString(u) {
					apiErr(w, 400, "invalid_config", fmt.Sprintf("webhook url must be http(s): %s", jsDisplay(urlV)))
					return
				}
				hook := WebhookConfig{URL: u}
				if evV, evPresent := wm["events"]; evPresent {
					evArr, evIsArr := evV.([]any)
					if !evIsArr {
						apiErr(w, 400, "invalid_config", "webhook events must be a list of non-empty strings")
						return
					}
					events := make([]string, 0, len(evArr))
					for _, e := range evArr {
						s, isStr := e.(string)
						if !isStr || strings.TrimSpace(s) == "" {
							apiErr(w, 400, "invalid_config", "webhook events must be a list of non-empty strings")
							return
						}
						events = append(events, s)
					}
					hook.Events = &events
				}
				hooks = append(hooks, hook)
			}
			cfg.Webhooks = hooks
		}
		persistConfig()
		writeJSON(w, 200, struct {
			OK bool `json:"ok"`
		}{true})
	}))

	mux.HandleFunc("GET /worktrees", need("read", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, struct {
			Worktrees []KeptWorktree `json:"worktrees"`
		}{nonNil(listKeptWorktrees())})
	}))

	mux.HandleFunc("DELETE /worktrees", need("admin", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Query().Get("path")
		if path == "" {
			apiErr(w, 400, "missing_param", "path required")
			return
		}
		if err := forceRemoveWorktree(path); err != nil {
			apiErr(w, 400, "worktree_error", err.Error())
			return
		}
		writeJSON(w, 200, struct {
			OK bool `json:"ok"`
		}{true})
	}))

	mux.HandleFunc("GET /sessions", need("read", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, struct {
			Sessions []Session     `json:"sessions"`
			Lost     []LostSession `json:"lost"`
		}{nonNil(listSessions()), nonNil(listLostSessions())})
	}))

	mux.HandleFunc("GET /journal/recent", need("read", func(w http.ResponseWriter, r *http.Request) {
		// Ceil: a fractional ?limit=2.5 yields 3 entries, like the TS < comparison
		limit := math.Ceil(math.Min(20, math.Max(1, jsNumberOr(r.URL.Query().Get("limit"), 5))))
		writeJSON(w, 200, struct {
			Recent []RecentLaunch `json:"recent"`
		}{nonNil(recentLaunches(int(limit)))})
	}))

	// Live lifecycle stream (SSE): session.start/ready/exit/kill as they happen.
	// No replay — reconnecting clients should re-list first; the journal is the history.
	mux.HandleFunc("GET /events", need("read", func(w http.ResponseWriter, r *http.Request) {
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
		ch := make(chan LifecycleEvent, 64)
		unsub := onEvent(func(e LifecycleEvent) {
			select {
			case ch <- e:
			default:
			}
		})
		defer unsub()

		hello, _ := json.Marshal(struct {
			At string `json:"at"`
		}{nowISO()})
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

	mux.HandleFunc("POST /sessions", need("launch", func(w http.ResponseWriter, r *http.Request) {
		body, ok := readJSONBody(r)
		if !ok {
			apiErr(w, 400, "invalid_json", "request body must be valid JSON")
			return
		}
		if !jsTruthy(body["folder"]) {
			apiErr(w, 400, "missing_param", "folder required")
			return
		}
		folder := strField(body["folder"])
		if !withinRoots(folder) {
			apiErr(w, 400, "outside_roots", "folder outside configured roots")
			return
		}
		var callbackURL string
		if cb, present := body["callbackUrl"]; present {
			s, isStr := cb.(string)
			if !isStr || !httpURL.MatchString(s) {
				apiErr(w, 400, "invalid_param", "callbackUrl must be an http(s) URL")
				return
			}
			callbackURL = s
		}

		session, err := createSession(createSessionOpts{
			folder:         folder,
			name:           strField(body["name"]),
			spawnMode:      strField(body["spawnMode"]),
			branch:         strField(body["branch"]),
			permissionMode: strField(body["permissionMode"]),
			callbackURL:    callbackURL,
			actor:          actorOf(r).name,
		})
		if err != nil {
			apiErr(w, 409, "launch_failed", err.Error())
			return
		}

		if r.URL.Query().Get("wait") != "ready" {
			writeJSON(w, 201, session)
			return
		}

		// ?wait=ready: one round-trip to a pairing URL for scripts and automations
		timeoutMs := math.Min(300000, math.Max(1000, jsNumberOr(body["timeoutMs"], 60000)))
		outcome := waitForReady(session.ID, time.Duration(timeoutMs*float64(time.Millisecond)))
		latest := session
		if s := getSession(session.ID); s != nil {
			latest = *s
		}
		switch outcome {
		case "ready":
			writeJSON(w, 201, latest)
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

	mux.HandleFunc("POST /jobs", need("launch", func(w http.ResponseWriter, r *http.Request) {
		body, ok := readJSONBody(r)
		if !ok {
			apiErr(w, 400, "invalid_json", "request body must be valid JSON")
			return
		}
		if !jsTruthy(body["folder"]) {
			apiErr(w, 400, "missing_param", "folder required")
			return
		}
		prompt, isStr := body["prompt"].(string)
		if !isStr || strings.TrimSpace(prompt) == "" {
			apiErr(w, 400, "missing_param", "prompt required")
			return
		}
		folder := strField(body["folder"])
		if !withinRoots(folder) {
			apiErr(w, 400, "outside_roots", "folder outside configured roots")
			return
		}
		var callbackURL string
		if cb, present := body["callbackUrl"]; present {
			s, isStr := cb.(string)
			if !isStr || !httpURL.MatchString(s) {
				apiErr(w, 400, "invalid_param", "callbackUrl must be an http(s) URL")
				return
			}
			callbackURL = s
		}
		// honest about the launch console's wired-for-v1 toggle: accepted, never ignored
		if jsTruthy(body["isolation"]) || jsTruthy(body["docker"]) {
			apiErr(w, 501, "not_implemented", "Docker isolation is not implemented yet — jobs run as the runner's user")
			return
		}
		// absent → -1 so jobs.go applies its default; an explicit 0 clamps to the 1s floor
		timeoutMs := -1
		if v, present := body["timeoutMs"]; present && v != nil {
			timeoutMs = int(jsNumberOr(v, 0))
		}
		job, err := createJob(createJobOpts{
			folder:         folder,
			prompt:         prompt,
			spawnMode:      strField(body["spawnMode"]),
			branch:         strField(body["branch"]),
			permissionMode: strField(body["permissionMode"]),
			timeoutMs:      timeoutMs,
			callbackURL:    callbackURL,
			actor:          actorOf(r).name,
		})
		if err != nil {
			apiErr(w, 409, "launch_failed", err.Error())
			return
		}
		writeJSON(w, 202, job)
	}))

	mux.HandleFunc("GET /jobs", need("read", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, struct {
			Jobs []Job `json:"jobs"`
		}{nonNil(listJobs())})
	}))

	mux.HandleFunc("GET /jobs/{id}", need("read", func(w http.ResponseWriter, r *http.Request) {
		job := getJob(r.PathValue("id"))
		if job == nil {
			apiErr(w, 404, "not_found", "no such job")
			return
		}
		writeJSON(w, 200, job)
	}))

	mux.HandleFunc("GET /jobs/{id}/log", need("read", func(w http.ResponseWriter, r *http.Request) {
		log := getJobLog(r.PathValue("id"))
		if log == nil {
			apiErr(w, 404, "not_found", "no such job")
			return
		}
		writeText(w, 200, *log)
	}))

	mux.HandleFunc("DELETE /jobs/{id}", need("launch", func(w http.ResponseWriter, r *http.Request) {
		job := cancelJob(r.PathValue("id"), actorOf(r).name)
		if job == nil {
			apiErr(w, 404, "not_found", "no such job")
			return
		}
		writeJSON(w, 200, job)
	}))

	mux.HandleFunc("DELETE /jobs/{id}/record", need("launch", func(w http.ResponseWriter, r *http.Request) {
		removed, err := removeJob(r.PathValue("id"))
		if err != nil {
			apiErr(w, 409, "job_live", err.Error())
			return
		}
		if !removed {
			apiErr(w, 404, "not_found", "no such job")
			return
		}
		writeJSON(w, 200, struct {
			OK bool `json:"ok"`
		}{true})
	}))

	mux.HandleFunc("GET /sessions/{id}", need("read", func(w http.ResponseWriter, r *http.Request) {
		session := getSession(r.PathValue("id"))
		if session == nil {
			apiErr(w, 404, "not_found", "no such session")
			return
		}
		writeJSON(w, 200, session)
	}))

	mux.HandleFunc("GET /sessions/{id}/qr", need("read", func(w http.ResponseWriter, r *http.Request) {
		session := getSession(r.PathValue("id"))
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

	mux.HandleFunc("GET /sessions/{id}/log", need("read", func(w http.ResponseWriter, r *http.Request) {
		log := getSessionLog(r.PathValue("id"))
		if log == nil {
			apiErr(w, 404, "not_found", "no such session")
			return
		}
		writeText(w, 200, *log)
	}))

	mux.HandleFunc("DELETE /sessions/{id}", need("launch", func(w http.ResponseWriter, r *http.Request) {
		session := killSession(r.PathValue("id"), actorOf(r).name)
		if session == nil {
			apiErr(w, 404, "not_found", "no such session")
			return
		}
		writeJSON(w, 200, session)
	}))

	mux.HandleFunc("DELETE /sessions/{id}/record", need("launch", func(w http.ResponseWriter, r *http.Request) {
		removed, err := removeSession(r.PathValue("id"))
		if err != nil {
			apiErr(w, 409, "session_live", err.Error())
			return
		}
		if !removed {
			apiErr(w, 404, "not_found", "no such session")
			return
		}
		writeJSON(w, 200, struct {
			OK bool `json:"ok"`
		}{true})
	}))

	return authMiddleware(cfg, mux)
}

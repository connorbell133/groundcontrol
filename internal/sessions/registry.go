package sessions

/* ---------- registry enrichment: claude agents --json ----------
One Manager-owned poll loop joins the CLI's live-session registry to launched
sessions: the Claude conversation UUID (journaled once — the id a future
resume needs), a busy/idle activity signal, and the other sessions running in
a launch's directory. Everything here is enrichment: the PTY stays the sole
exit authority (R6), and every failure degrades to absent fields, never to a
state transition or an error. The loop is run-to-completion-then-sleep — the
next wait is armed only after a tick finishes, so overlapping execs are
impossible by construction. */

import (
	"context"
	"log"
	"math/rand"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"github.com/connorbell133/groundcontrol/internal/claudex"
)

// Cadence tiers. Package vars, not consts, so tests can shrink them
// (precedent: transcripts.go's claudeProjectsDir). One registry exec costs
// ~200ms wall / ~300MB transient RSS (measured 2026-07-15), which is why the
// loop is gated on live sessions and tiered instead of a flat fast poll.
var (
	registryFastInterval = 3 * time.Second
	registrySlowInterval = 25 * time.Second
	registryFailureCap   = time.Minute
	// the floor under the retention window, so a fast cadence doesn't turn
	// one blip into an instant clear
	registryGraceFloor = 10 * time.Second
)

const (
	// a GET /sessions inside this window means someone is watching — poll fast
	registryObservedWindow = 10 * time.Second
	// forced fast tier while a session still lacks its UUID, bounded per
	// session: chase the first capture hard early, then stop paying for a
	// UUID that may never appear (old CLI, launch that never paired)
	registryUUIDChaseWindow = 2 * time.Minute
	// `claude agents --json` first shipped in this CLI release
	registryVersionFloor = "2.1.145"
	// bound on the pid→ppid walk; the observed topology is launcher → forked
)

// Exec seams, swappable in tests so registry scenarios need no real CLI or
// process tree (precedent: transcripts.go's claudeProjectsDir var).
var (
	registryQuery        = func(timeout time.Duration) ([]claudex.Agent, error) { return claudex.Agents(timeout) }
	registryProbeVersion = func() (string, error) { return claudex.Version(claudex.DefaultTimeout) }
)

// psParentMap yields the pid→ppid map for one tick (shared exec in ps.go).
// nil means "unavailable" and switches the join to the last-resort cwd
// fallback; a package var so tests inject synthetic process trees.
var psParentMap = func() map[int]int {
	ps, err := execPSParents(3 * time.Second)
	if err != nil || len(ps) == 0 {
		return nil
	}
	return ps
}

// MarkObserved records that an API reader just listed sessions — the signal
// that flips the poll cadence to the fast tier and cuts a slow sleep short.
func (m *Manager) MarkObserved() {
	m.mu.Lock()
	m.observedAt = time.Now()
	m.mu.Unlock()
	select {
	case m.regWake <- struct{}{}:
	default:
	}
}

// StartRegistryLoop arms the poller with the process's signal context. The
// loop itself runs only while at least one session is live: Create starts it
// on the 0→1 transition and it parks itself again after the last exit — zero
// registry execs on an idle runner.
func (m *Manager) StartRegistryLoop(ctx context.Context) {
	m.mu.Lock()
	m.regCtx = ctx
	start := false
	for _, s := range m.sessions {
		if s.State == StateStarting || s.State == StateReady {
			start = true
			break
		}
	}
	start = start && !m.regRunning
	if start {
		m.regRunning = true
	}
	m.mu.Unlock()
	if start {
		go m.registryLoop(ctx)
	}
}

func (m *Manager) stopRegistryLoop() {
	m.mu.Lock()
	m.regRunning = false
	m.mu.Unlock()
}

func (m *Manager) registryLoop(ctx context.Context) {
	if v, err := registryProbeVersion(); err == nil && !claudex.AtLeast(v, registryVersionFloor) {
		// old CLI: agents --json doesn't exist. Degrade to absent enrichment
		// instead of hammering a subcommand that can't answer; the next 0→1
		// transition re-probes, so a CLI upgrade needs no runner restart.
		log.Printf("sessions: claude %s lacks agents --json (needs %s) — registry enrichment off", v, registryVersionFloor)
		m.stopRegistryLoop()
		return
	}
	interval := registryFastInterval
	failures := 0
	for {
		if ctx.Err() != nil {
			m.stopRegistryLoop()
			return
		}
		snaps, fast, ok := m.registrySnapshot(time.Now())
		if !ok {
			return // last session exited; registrySnapshot already released loop ownership
		}

		// exec, decode, and the ps snapshot all run outside m.mu — a wedged
		// CLI must never stall API reads
		rows, err := registryQuery(claudex.DefaultTimeout)
		now := time.Now()
		if err != nil {
			failures++
			next := nextRegistryInterval(interval, fast, true)
			if failures == 1 || next != interval {
				// rate-limited: the first failure, then once per doubling step
				log.Printf("sessions: registry poll failed (%v); next attempt in %s", err, next)
			}
			interval = next
			m.ageRegistryState(retentionWindow(interval), now)
		} else {
			failures = 0
			interval = nextRegistryInterval(interval, fast, false)
			ps := psParentMap()
			// symlink-normalize both sides of the cwd comparison out here:
			// EvalSymlinks does filesystem work that must not run under m.mu
			for i := range snaps {
				snaps[i].cwd = normalizePath(snaps[i].cwd)
			}
			normRows := append([]claudex.Agent(nil), rows...)
			for i := range normRows {
				normRows[i].Cwd = normalizePath(normRows[i].Cwd)
			}
			joins := joinRegistry(snaps, normRows, ps)
			captures, scans := m.applyRegistryTick(snaps, joins, ps != nil, retentionWindow(interval), now)
			for _, c := range captures {
				// journaled, never announced: the UUID is a durable fact the
				// UI polls for, not a lifecycle event to fan out (R8)
				m.journal.Append(map[string]any{"event": evSessionClaudeID, "id": c.id, "claudeSessionId": c.uuid})
			}
			m.applyPRLinks(scans)
		}

		if !sleepInterrupt(ctx, m.regWake, jittered(interval), registryFastInterval) {
			m.stopRegistryLoop()
			return
		}
	}
}

// regSessionSnap is the slice of a live session one tick joins against.
type regSessionSnap struct {
	id        string
	pid       int // the spawned claude launcher; registry rows are its descendants
	cwd       string
	startedAt time.Time
	uuid      string // captured conversation id, "" until first capture
}

// registrySnapshot collects what one tick joins against, under one short
// lock. ok=false means no session is live: loop ownership is released under
// the same lock, so a concurrent Create sees either a running loop or none —
// never a stale flag.
func (m *Manager) registrySnapshot(now time.Time) (snaps []regSessionSnap, fast bool, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.State != StateStarting && s.State != StateReady {
			continue
		}
		started, _ := time.Parse(time.RFC3339, s.StartedAt)
		snap := regSessionSnap{id: s.ID, cwd: s.cwd, startedAt: started}
		if s.cmd != nil && s.cmd.Process != nil {
			snap.pid = s.cmd.Process.Pid
		}
		if s.ClaudeSessionID != nil {
			snap.uuid = *s.ClaudeSessionID
		} else if now.Sub(started) < registryUUIDChaseWindow {
			fast = true
		}
		snaps = append(snaps, snap)
	}
	if len(snaps) == 0 {
		m.regRunning = false
		return nil, false, false
	}
	if now.Sub(m.observedAt) <= registryObservedWindow {
		fast = true
	}
	return snaps, fast, true
}

// nextRegistryInterval implements the tiered cadence: fast while watched or
// chasing a first UUID capture, slow otherwise; consecutive failures double
// the previous interval up to a cap so a broken CLI costs one exec a minute,
// not twenty. Any success resets to the tier the tick computed.
func nextRegistryInterval(prev time.Duration, fast, failed bool) time.Duration {
	if failed {
		next := prev * 2
		if next > registryFailureCap {
			next = registryFailureCap
		}
		return next
	}
	if fast {
		return registryFastInterval
	}
	return registrySlowInterval
}

// retentionWindow is how long activity and extras survive without a fresh
// confirm — wall-clock, not tick counts, so the grace rules hold across
// cadence changes.
func retentionWindow(interval time.Duration) time.Duration {
	if w := 2 * interval; w > registryGraceFloor {
		return w
	}
	return registryGraceFloor
}

// jittered spreads ticks by up to 10% so several runners on one box don't
// synchronize their execs.
func jittered(d time.Duration) time.Duration {
	return d + time.Duration(rand.Int63n(int64(d)/10+1))
}

// sleepInterrupt waits out one interval, ending early (down to floor) when a
// reader pokes wake — so the first watched tick after an idle stretch doesn't
// sit out a slow interval. Returns false when ctx ends the loop.
func sleepInterrupt(ctx context.Context, wake <-chan struct{}, d, floor time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	deadline := time.Now().Add(d)
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case <-wake:
			if remaining := time.Until(deadline); remaining > floor {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(floor)
				deadline = time.Now().Add(floor)
			}
		}
	}
}

// normalizePath resolves symlinks so registry cwds and session cwds compare
// equal (macOS /tmp vs /private/tmp); a path that fails to resolve still
// compares by its cleaned literal form.
func normalizePath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
}

// normalizeActivity keeps the two observed values and maps anything else to
// absent: status is observed-not-enumerated, so an unknown value must read as
// "no signal", never as a new state (R3/R7).
func normalizeActivity(status string) string {
	if status == "busy" || status == "idle" {
		return status
	}
	return ""
}

// extraRow is one tick's sighting of a registry row attached to a session.
// Ownership is decided here, at join time — the only point where descendant
// vs same-folder is knowable — and carried through to the wire split.
type extraRow struct {
	key     string // stable identity for wall-clock aging
	name    string
	status  string
	owned   bool // descendant of the spawned pid — an environment row
	primary bool // the environment's primary session, emitted explicitly
}

// regJoin is one session's slice of a tick's join result.
type regJoin struct {
	rowSeen  bool
	uuid     string
	activity string
	extras   []extraRow
}

// joinRegistry is the pure per-tick join over (session snapshots, registry
// rows, pid→ppid map); cwds must be pre-normalized. Row ownership resolves in
// order of contract strength: a row carrying a session's captured UUID is
// that session; otherwise a row belongs to the launch whose spawned pid its
// ancestry walk reaches; with no ps map at all, a cwd+startedAt pair may bind
// only when it is unambiguous from both sides — a wrong guess here would be
// journaled forever, so ambiguity means no capture. The primary row is the
// earliest-started owned row; it and later owned rows surface as environment
// rows (owned, primary first), unowned rows in a session's folder as folder
// rows — never as joins.
func joinRegistry(snaps []regSessionSnap, rows []claudex.Agent, ps map[int]int) map[string]*regJoin {
	joins := make(map[string]*regJoin, len(snaps))
	for i := range snaps {
		joins[snaps[i].id] = &regJoin{}
	}
	owner := make([]int, len(rows)) // row index → snap index, -1 unowned
	for i := range owner {
		owner[i] = -1
	}
	for ri := range rows {
		if rows[ri].SessionID == "" {
			continue
		}
		for si := range snaps {
			if snaps[si].uuid != "" && snaps[si].uuid == rows[ri].SessionID {
				owner[ri] = si
				break
			}
		}
	}
	if ps != nil {
		for ri := range rows {
			if owner[ri] != -1 {
				continue
			}
			for si := range snaps {
				if reachesAncestor(ps, rows[ri].PID, snaps[si].pid) {
					owner[ri] = si
					break
				}
			}
		}
	} else {
		rowCands := make([][]int, len(rows))
		snapCands := make([][]int, len(snaps))
		for ri := range rows {
			if owner[ri] != -1 {
				continue
			}
			for si := range snaps {
				if rows[ri].Cwd == snaps[si].cwd && rows[ri].StartedAt >= snaps[si].startedAt.UnixMilli() {
					rowCands[ri] = append(rowCands[ri], si)
					snapCands[si] = append(snapCands[si], ri)
				}
			}
		}
		for ri := range rows {
			if owner[ri] == -1 && len(rowCands[ri]) == 1 && len(snapCands[rowCands[ri][0]]) == 1 {
				owner[ri] = rowCands[ri][0]
			}
		}
	}

	for si := range snaps {
		var owned []int
		for ri := range rows {
			if owner[ri] == si {
				owned = append(owned, ri)
			}
		}
		if len(owned) == 0 {
			continue
		}
		sort.Slice(owned, func(a, b int) bool {
			if rows[owned[a]].StartedAt != rows[owned[b]].StartedAt {
				return rows[owned[a]].StartedAt < rows[owned[b]].StartedAt
			}
			return rows[owned[a]].PID < rows[owned[b]].PID
		})
		j := joins[snaps[si].id]
		primary := rows[owned[0]]
		j.rowSeen = true
		j.uuid = primary.SessionID
		j.activity = normalizeActivity(primary.Status)
		// the primary is itself an environment row, emitted explicitly: its
		// status reads the same registry row as the card-level activity, so
		// the two surfaces can never disagree
		p := extraRowOf(primary)
		p.owned, p.primary = true, true
		j.extras = append(j.extras, p)
		for _, ri := range owned[1:] {
			r := extraRowOf(rows[ri])
			r.owned = true
			j.extras = append(j.extras, r)
		}
	}
	// unowned rows in a session's folder are visible as extras on every
	// same-cwd launch — a row owned by launch A never bleeds into B's list
	for ri := range rows {
		if owner[ri] != -1 {
			continue
		}
		for si := range snaps {
			if rows[ri].Cwd == snaps[si].cwd {
				j := joins[snaps[si].id]
				j.extras = append(j.extras, extraRowOf(rows[ri]))
			}
		}
	}
	return joins
}

func extraRowOf(a claudex.Agent) extraRow {
	key := a.SessionID
	if key == "" {
		key = a.Name
	}
	if key == "" {
		key = strconv.Itoa(a.PID)
	}
	// live registry rows can lack a name entirely (observed: IDE/SDK sessions
	// and fresh pre-created ones) — fall back to a real identifier so the card
	// never renders a blank row
	name := a.Name
	if name == "" && len(a.SessionID) >= 8 {
		name = a.SessionID[:8]
	}
	if name == "" {
		name = "pid " + strconv.Itoa(a.PID)
	}
	return extraRow{key: key, name: name, status: normalizeActivity(a.Status)}
}

type claudeIDCapture struct {
	id, uuid string
}

// extraRecord is the aging state behind one wire row, carrying the
// classification decided at join time so it can hold across ps outages.
type extraRecord struct {
	name, status string
	owned        bool // environment row vs same-folder foreign
	primary      bool // the environment's primary session — sorts first
	seen         time.Time
}

// applyRegistryTick writes one successful tick's join into the live table
// under a single lock acquisition, and collects the transcript-scan inputs
// for the pr-link metadata pass. The state guard re-checks each session's
// state here because the registry snapshot predates this lock: a session that
// exited in between takes no writes at all — activity must never resurrect on
// a dead card, and a reaped pid can already belong to a stranger, so even a
// first UUID capture is dropped. psOK reports whether the tick had a ps
// snapshot: only then is ownership freshly provable, so only then may a
// row's classification change or an environment row clear on absence.
func (m *Manager) applyRegistryTick(snaps []regSessionSnap, joins map[string]*regJoin, psOK bool, window time.Duration, now time.Time) ([]claudeIDCapture, []prScanInput) {
	captures := []claudeIDCapture{}
	scans := []prScanInput{}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range snaps {
		s, ok := m.sessions[snaps[i].id]
		if !ok || (s.State != StateStarting && s.State != StateReady) {
			continue
		}
		j := joins[snaps[i].id]
		if j == nil {
			continue
		}
		if j.rowSeen {
			if j.activity != "" {
				a := j.activity
				s.Activity = &a
			} else {
				s.Activity = nil // unknown status value reads as absent, not as an error
			}
			s.activitySeenAt = now
			if j.uuid != "" {
				switch {
				case s.ClaudeSessionID == nil:
					u := j.uuid
					s.ClaudeSessionID = &u
					captures = append(captures, claudeIDCapture{id: s.ID, uuid: u})
				case *s.ClaudeSessionID != j.uuid && !s.claudeIDConflictLogged:
					// first capture wins — the journal already carries it;
					// log the disagreement once, not on every tick
					s.claudeIDConflictLogged = true
					log.Printf("sessions: registry sessionId %s disagrees with captured %s for session %s — keeping the first capture", j.uuid, *s.ClaudeSessionID, s.ID)
				}
			}
		} else {
			// the registry answered and this session's row is gone: absence
			// is authoritative — clear now, no grace (R3's no-stale rule)
			s.Activity = nil
		}
		if s.extrasSeen == nil {
			s.extrasSeen = map[string]extraRecord{}
		}
		tickKeys := make(map[string]bool, len(j.extras))
		for _, e := range j.extras {
			rec := extraRecord{name: e.name, status: e.status, owned: e.owned, primary: e.primary, seen: now}
			if !psOK && !e.owned {
				// sticky ownership: without a ps snapshot the join can't prove
				// ancestry, so a row it now reads as foreign keeps its last
				// confident classification — demoting an environment row to
				// the folder group would be a wrong answer, not a degraded one
				if prev, ok := s.extrasSeen[e.key]; ok && prev.owned {
					rec.owned, rec.primary = true, prev.primary
				}
			}
			s.extrasSeen[e.key] = rec
			tickKeys[e.key] = true
		}
		if psOK {
			// registry and ps both answered: absence is authoritative for
			// environment rows — clear now, no grace, mirroring the activity
			// rule above. Folder rows keep the wall-clock aging; without ps,
			// environment rows fall back to it too.
			for k, rec := range s.extrasSeen {
				if rec.owned && !tickKeys[k] {
					delete(s.extrasSeen, k)
				}
			}
		}
		s.refreshExtrasLocked(window, now)
		if s.ClaudeSessionID != nil {
			// the transcript filename is exactly the captured conversation
			// UUID inside the launch cwd's munged project dir (transcripts.go)
			scans = append(scans, prScanInput{
				id:       s.ID,
				path:     filepath.Join(claudeProjectsDir, claudeProjectDirName(s.cwd), *s.ClaudeSessionID+".jsonl"),
				lastSize: s.prStatSize,
			})
		}
	}
	return captures, scans
}

// prScanInput is one session's pr-link scan work order for a tick.
type prScanInput struct {
	id       string
	path     string
	lastSize int64
}

// applyPRLinks runs the transcript metadata pass for sessions with a captured
// UUID: the stat/scan I/O happens outside the lock, then one lock acquisition
// writes under the same state guard as every other registry write. A rescan's
// answer is authoritative for the file — including nil, so a vanished record
// (or a vanished transcript) clears the link rather than pinning a stale one.
func (m *Manager) applyPRLinks(scans []prScanInput) {
	if len(scans) == 0 {
		return
	}
	type prResult struct {
		id        string
		link      *PRLink
		size      int64
		rescanned bool
	}
	results := make([]prResult, 0, len(scans))
	for _, in := range scans {
		link, size, rescanned := prLinkFromTranscript(in.path, in.lastSize)
		results = append(results, prResult{id: in.id, link: link, size: size, rescanned: rescanned})
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range results {
		s, ok := m.sessions[r.id]
		if !ok || (s.State != StateStarting && s.State != StateReady) {
			continue
		}
		s.prStatSize = r.size
		if r.rescanned {
			s.PRLink = r.link
		}
	}
}

// ageRegistryState is the failed-tick path: nothing joined, so activity and
// extras ride out the wall-clock grace window and then clear — degrade to
// absence, never to a stale answer (R3).
func (m *Manager) ageRegistryState(window time.Duration, now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.State != StateStarting && s.State != StateReady {
			continue
		}
		if s.Activity != nil && now.Sub(s.activitySeenAt) > window {
			s.Activity = nil
		}
		s.refreshExtrasLocked(window, now)
	}
}

// refreshExtrasLocked rebuilds the two wire slices from the sighting map,
// dropping entries unconfirmed past the window. Always fresh slices: Session
// snapshots escape the lock, so a slice already handed out must stay
// immutable. An empty list is nil — absence on the wire, never []. Caller
// holds m.mu.
func (s *liveSession) refreshExtrasLocked(window time.Duration, now time.Time) {
	for k, rec := range s.extrasSeen {
		if now.Sub(rec.seen) > window {
			delete(s.extrasSeen, k)
		}
	}
	var env []extraRecord
	var folder []ExtraSession
	for _, rec := range s.extrasSeen {
		if rec.owned {
			env = append(env, rec)
		} else {
			folder = append(folder, ExtraSession{Name: rec.name, Status: rec.status})
		}
	}
	s.EnvironmentSessions, s.FolderSessions = nil, nil
	if len(env) > 0 {
		sort.Slice(env, func(a, b int) bool {
			if env[a].primary != env[b].primary {
				return env[a].primary
			}
			return env[a].name < env[b].name
		})
		out := make([]ExtraSession, len(env))
		for i, rec := range env {
			out[i] = ExtraSession{Name: rec.name, Status: rec.status}
		}
		s.EnvironmentSessions = out
	}
	if len(folder) > 0 {
		sort.Slice(folder, func(a, b int) bool { return folder[a].Name < folder[b].Name })
		s.FolderSessions = folder
	}
}

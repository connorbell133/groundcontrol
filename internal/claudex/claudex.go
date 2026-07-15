// Package claudex wraps the claude CLI's state-query surfaces — the agents
// registry and the version probe — behind hard-timeout execs and tolerant
// decoding, so a wedged or drifting CLI degrades to absent enrichment instead
// of errors or hangs. Scope boundary: claudex owns claude state queries only;
// the interactive PTY spawn (sessions) and the one-shot prompt exec (jobs)
// stay with their owners.
package claudex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout bounds every claude exec: registry callers run on poll loops
// and launch paths that must never wait on a stuck CLI longer than a tick.
const DefaultTimeout = 5 * time.Second

// Agent is one row of `claude agents --json`, decoding only the fields
// GroundControl joins on — the registry schema is documented in prose, not a
// contract, so unknown fields are ignored and missing ones read zero-valued.
type Agent struct {
	PID       int    `json:"pid"`
	Cwd       string `json:"cwd"`
	SessionID string `json:"sessionId"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	StartedAt int64  `json:"startedAt"` // unix milliseconds
}

// Agents queries the live-session registry. The slice is always non-nil so
// callers can range and join without a nil guard; on any failure (exec error,
// timeout, non-JSON stdout such as an update banner) it is empty alongside
// the error — never a partial guess.
func Agents(timeout time.Duration) ([]Agent, error) {
	rows := []Agent{}
	out, err := run(timeout, "agents", "--json")
	if err != nil {
		return rows, err
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		return []Agent{}, fmt.Errorf("claude agents --json: non-JSON output: %w", err)
	}
	if rows == nil {
		// a literal "null" would otherwise smuggle nil past the non-nil contract
		rows = []Agent{}
	}
	return rows, nil
}

// Version probes the CLI and returns the normalized "X.Y.Z" triple. Output it
// can't parse (wrapper scripts, error text on stdout) is an error so feature
// gates fail closed rather than assuming a modern CLI.
func Version(timeout time.Duration) (string, error) {
	out, err := run(timeout, "--version")
	if err != nil {
		return "", err
	}
	v, ok := parseTriple(out)
	if !ok {
		return "", fmt.Errorf("claude --version: unrecognized output %q", out)
	}
	return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]), nil
}

// AtLeast reports whether version meets floor. Either side failing to parse
// returns false: version-floored features stay off when the CLI's answer is
// garbage, matching the degrade-to-absence rule everywhere else.
func AtLeast(version, floor string) bool {
	v, okV := parseTriple(version)
	f, okF := parseTriple(floor)
	if !okV || !okF {
		return false
	}
	for i := range v {
		if v[i] != f[i] {
			return v[i] > f[i]
		}
	}
	return true
}

// parseTriple accepts both `X.Y.Z (Claude Code)` and bare `X.Y.Z` — the CLI
// has printed both shapes across releases.
func parseTriple(s string) (v [3]int, ok bool) {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return v, false
	}
	parts := strings.Split(fields[0], ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// run executes claude <args...> with a hard timeout, mirroring gitx.Exec:
// on failure the last non-empty stderr line is the error — the CLI's own
// message is the most useful thing to surface.
func run(timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Env = append(os.Environ(), "FORCE_COLOR=0", "NO_COLOR=1")
	// a killed CLI can leave a spawned child holding the inherited stdout
	// pipe; without WaitDelay, Wait would block on that pipe past the timeout
	cmd.WaitDelay = time.Second
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("claude %s: %w", strings.Join(args, " "), ctx.Err())
		}
		stderrLine := ""
		for _, line := range strings.Split(errBuf.String(), "\n") {
			if l := strings.TrimSpace(line); l != "" {
				stderrLine = l
			}
		}
		if stderrLine != "" {
			return "", errors.New(stderrLine)
		}
		return "", err
	}
	return strings.TrimSpace(out.String()), nil
}

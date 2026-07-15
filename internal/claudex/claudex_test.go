package claudex_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/connorbell133/groundcontrol/internal/claudex"
	"github.com/connorbell133/groundcontrol/internal/testutil"
)

// Every test here rewrites PATH via t.Setenv, so none may call t.Parallel.

func TestAgentsDecodesRowsIgnoringUnknownFields(t *testing.T) {
	testutil.FakeClaudeWith(t, testutil.FakeClaudeConfig{AgentsJSON: `[
		{"pid":101,"cwd":"/tmp/wt-a","sessionId":"uuid-aaa","name":"gc-alpha","status":"busy","startedAt":1752570000000,"model":"opus","transcript":{"path":"/x"}},
		{"pid":202,"cwd":"/tmp/wt-b","sessionId":"uuid-bbb","name":"gc-beta","status":"idle","startedAt":1752570001000,"futureField":true}
	]`})
	rows, err := claudex.Agents(claudex.DefaultTimeout)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	want := []claudex.Agent{
		{PID: 101, Cwd: "/tmp/wt-a", SessionID: "uuid-aaa", Name: "gc-alpha", Status: "busy", StartedAt: 1752570000000},
		{PID: 202, Cwd: "/tmp/wt-b", SessionID: "uuid-bbb", Name: "gc-beta", Status: "idle", StartedAt: 1752570001000},
	}
	if len(rows) != len(want) {
		t.Fatalf("got %d rows, want %d: %+v", len(rows), len(want), rows)
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, rows[i], want[i])
		}
	}
}

func TestAgentsNonJSONOutputDegradesToEmptyWithError(t *testing.T) {
	testutil.FakeClaudeWith(t, testutil.FakeClaudeConfig{
		AgentsJSON: "New Claude Code version available!\nRun claude update to upgrade.",
	})
	rows, err := claudex.Agents(claudex.DefaultTimeout)
	if err == nil {
		t.Fatal("want error for non-JSON stdout, got nil")
	}
	if rows == nil || len(rows) != 0 {
		t.Fatalf("want empty non-nil rows, got %#v", rows)
	}
}

func TestAgentsEmptyArrayReturnsEmptyNonNilSlice(t *testing.T) {
	testutil.FakeClaudeWith(t, testutil.FakeClaudeConfig{})
	rows, err := claudex.Agents(claudex.DefaultTimeout)
	if err != nil {
		t.Fatalf("Agents: %v", err)
	}
	if rows == nil {
		t.Fatal("want non-nil slice for empty registry, got nil")
	}
	if len(rows) != 0 {
		t.Fatalf("want 0 rows, got %+v", rows)
	}
}

func TestAgentsTimeoutSurfacesWithinBound(t *testing.T) {
	// exec replaces the shell so the kill reaches the sleeper directly and no
	// orphan holds the stdout pipe open past the test
	fakeClaudeScript(t, "#!/bin/sh\nexec sleep 5\n")
	start := time.Now()
	rows, err := claudex.Agents(100 * time.Millisecond)
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want DeadlineExceeded, got %v", err)
	}
	if rows == nil || len(rows) != 0 {
		t.Fatalf("want empty non-nil rows on timeout, got %#v", rows)
	}
	// well under the stub's 5s sleep proves the timeout, not the child's
	// exit, unblocked the call
	if elapsed > 3*time.Second {
		t.Fatalf("timeout took %v, want well under the stub's 5s sleep", elapsed)
	}
}

func TestVersion(t *testing.T) {
	cases := []struct {
		name    string
		version string
		want    string
		wantErr bool
	}{
		{name: "suffixed", version: "2.1.172 (Claude Code)", want: "2.1.172"},
		{name: "bare", version: "2.1.207", want: "2.1.207"},
		{name: "garbage", version: "definitely not a version", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			testutil.FakeClaudeWith(t, testutil.FakeClaudeConfig{Version: tc.version})
			got, err := claudex.Version(claudex.DefaultTimeout)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Version: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Version = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAtLeast(t *testing.T) {
	cases := []struct {
		version, floor string
		want           bool
	}{
		{"2.1.172", "2.1.199", false},
		{"2.1.200", "2.1.200", true},
		{"2.1.210", "2.1.145", true},
		{"2.2.0", "2.1.999", true},
		{"1.9.9", "2.0.0", false},
		{"garbage", "2.1.199", false},
		{"2.1.200", "garbage", false},
		{"", "2.1.199", false},
	}
	for _, tc := range cases {
		if got := claudex.AtLeast(tc.version, tc.floor); got != tc.want {
			t.Errorf("AtLeast(%q, %q) = %v, want %v", tc.version, tc.floor, got, tc.want)
		}
	}
}

// fakeClaudeScript installs a bespoke claude stub for shapes FakeClaudeConfig
// doesn't model (hangs). Uses t.Setenv, so callers must not call t.Parallel.
func fakeClaudeScript(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake claude: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

package journal

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

func TestJournalRoundTrip(t *testing.T) {
	t.Parallel()
	j := New(t.TempDir())
	j.Append(map[string]any{"event": "first", "n": float64(1)})
	j.Append(map[string]any{"event": "second"})

	got := j.Read()
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0]["event"] != "first" || got[1]["event"] != "second" {
		t.Errorf("entries out of order: %v", got)
	}
	if got[0]["n"] != float64(1) {
		t.Errorf("value did not round-trip: %v", got[0]["n"])
	}
	for _, e := range got {
		if at, _ := e["at"].(string); at == "" {
			t.Errorf("entry missing at: %v", e)
		}
	}
}

func TestJournalSkipsGarbageLines(t *testing.T) {
	t.Parallel()
	j := New(t.TempDir())
	content := strings.Join([]string{
		`{"event":"a"}`,
		`{"torn":`, // crash mid-write
		`garbage not json`,
		`[1,2,3]`, // non-object
		`null`,
		`"just a string"`,
		`   `,
		`{"event":"b"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(j.path(), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := j.Read()
	if len(got) != 2 || got[0]["event"] != "a" || got[1]["event"] != "b" {
		t.Errorf("expected the two object lines to survive, got %v", got)
	}
}

func TestJournalReadCap(t *testing.T) {
	t.Parallel()
	j := New(t.TempDir())
	var b strings.Builder
	for i := 0; i < 2100; i++ {
		fmt.Fprintf(&b, "{\"i\":%d}\n", i)
	}
	if err := os.WriteFile(j.path(), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got := j.Read()
	if len(got) != 2000 {
		t.Fatalf("expected the 2000-entry cap, got %d", len(got))
	}
	if got[0]["i"] != float64(100) || got[len(got)-1]["i"] != float64(2099) {
		t.Errorf("cap should keep the newest entries: first=%v last=%v", got[0]["i"], got[len(got)-1]["i"])
	}
}

func TestJournalLegacyMigration(t *testing.T) {
	t.Parallel()
	j := New(t.TempDir())
	legacy := `[{"event":"a"},"stray string",{"event":"b"}]`
	if err := os.WriteFile(j.legacyPath(), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}

	got := j.Read()
	if len(got) != 2 || got[0]["event"] != "a" || got[1]["event"] != "b" {
		t.Fatalf("migrated entries wrong: %v", got)
	}
	if _, err := os.Stat(j.path()); err != nil {
		t.Errorf("journal.jsonl not created: %v", err)
	}
	if _, err := os.Stat(j.legacyPath() + ".bak"); err != nil {
		t.Errorf("legacy file not renamed to .bak: %v", err)
	}
	if _, err := os.Stat(j.legacyPath()); !os.IsNotExist(err) {
		t.Errorf("legacy journal.json should be gone, stat err = %v", err)
	}

	// appends keep working after migration
	j.Append(map[string]any{"event": "c"})
	got = j.Read()
	if len(got) != 3 || got[2]["event"] != "c" {
		t.Errorf("append after migration failed: %v", got)
	}
}

func TestJournalMigrationSkippedWhenJSONLExists(t *testing.T) {
	t.Parallel()
	j := New(t.TempDir())
	if err := os.WriteFile(j.path(), []byte(`{"event":"new"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(j.legacyPath(), []byte(`[{"event":"old"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	got := j.Read()
	if len(got) != 1 || got[0]["event"] != "new" {
		t.Errorf("existing JSONL must win over the legacy file: %v", got)
	}
	if _, err := os.Stat(j.legacyPath()); err != nil {
		t.Errorf("legacy file should be left untouched: %v", err)
	}
}

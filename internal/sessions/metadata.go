package sessions

/* ---------- transcript metadata: pr-link enrichment ----------
Claude Code appends a {"type":"pr-link","sessionId":"...","prNumber":9,
"prUrl":"https://github.com/..."} record to a conversation's JSONL transcript
when the session links a pull request. UNDOCUMENTED COUPLING: the record type
is a transcript internal (the docs explicitly call the JSONL format unstable)
and may vanish in any CLI release — absence is the contract (R5/R7). A
missing file, an unrecognized shape, or a removed record type all degrade to
"no PR link", never to an error. */

import (
	"encoding/json"
	"os"
)

// PRLink is the pull request a session's transcript most recently linked.
type PRLink struct {
	Number int    `json:"number"`
	URL    string `json:"url"`
}

// missingTranscriptSize is the stat-gate sentinel for "file absent" —
// distinct from every real size, so a transcript reappearing always triggers
// a rescan.
const missingTranscriptSize int64 = -1

// prLinkFromTranscript stat-gates a transcript scan: when the file size is
// unchanged since lastSize nothing is read, so the steady-state cost is one
// stat per session per tick. On change it runs a full linear rescan — no
// offset tracking on purpose: per-session offsets, truncation detection, and
// reset logic are disproportionate for best-effort enrichment, and a full
// rescan naturally recovers a record on a line that was torn mid-write during
// an earlier read. rescanned=false means "keep whatever you had"; size feeds
// the next call's gate.
func prLinkFromTranscript(path string, lastSize int64) (link *PRLink, size int64, rescanned bool) {
	st, err := os.Stat(path)
	if err != nil {
		// transcript gone (moved, cleaned): absence is the contract — report
		// the transition once so the caller clears the link, then settle
		if lastSize == missingTranscriptSize {
			return nil, missingTranscriptSize, false
		}
		return nil, missingTranscriptSize, true
	}
	if st.Size() == lastSize {
		return nil, lastSize, false
	}
	return scanPRLink(path), st.Size(), true
}

// scanPRLink returns the newest pr-link record in the transcript — the file
// is append-only, so last wins. Same tolerant posture as parseTranscript:
// unknown record types, torn lines, and non-JSON lines are skipped, and a
// scanner abort (a line past the 8MB buffer) still returns everything found
// before it.
func scanPRLink(path string) *PRLink {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	sc := transcriptScanner(f)
	var link *PRLink
	for sc.Scan() {
		var e struct {
			Type     string `json:"type"`
			PRNumber int    `json:"prNumber"`
			PRURL    string `json:"prUrl"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.Type != "pr-link" || e.PRURL == "" {
			continue
		}
		link = &PRLink{Number: e.PRNumber, URL: e.PRURL}
	}
	return link
}

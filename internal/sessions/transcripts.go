package sessions

/* ---------- conversation transcripts ----------
Claude Code writes one JSONL transcript per conversation under
~/.claude/projects/<munged-cwd>/. The remote-control host and every session the
phone adds inside it share the launch cwd, so that one directory holds the whole
launch's conversations. */

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/connorbell133/groundcontrol/internal/util"
)

var claudeProjectsDir = filepath.Join(util.MustHome(), ".claude", "projects")

// claudeProjectDirName mirrors Claude Code's path munging: every character
// outside [A-Za-z0-9] becomes "-".
func claudeProjectDirName(cwd string) string {
	var b strings.Builder
	for _, r := range cwd {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

type TranscriptMessage struct {
	Role string `json:"role"` // "user" | "assistant"
	Text string `json:"text"`
	At   string `json:"at"`
}

type Transcript struct {
	SessionID string              `json:"sessionId"` // Claude Code's conversation id, not ours
	UpdatedAt string              `json:"updatedAt"`
	Messages  []TranscriptMessage `json:"messages"`
}

const (
	maxTranscriptMessages = 200
	maxTranscriptText     = 4000
)

// transcriptText flattens a message's content: plain strings pass through,
// text blocks join, tool calls compress to "→ ToolName". Tool results and
// thinking blocks are noise at phone-log altitude and drop out.
func transcriptText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	parts := []string{}
	for _, b := range blocks {
		switch b.Type {
		case "text":
			if strings.TrimSpace(b.Text) != "" {
				parts = append(parts, b.Text)
			}
		case "tool_use":
			parts = append(parts, "→ "+b.Name)
		}
	}
	return strings.Join(parts, "\n")
}

func parseTranscript(path string) *Transcript {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	t := &Transcript{SessionID: strings.TrimSuffix(filepath.Base(path), ".jsonl")}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		var e struct {
			Type        string `json:"type"`
			Timestamp   string `json:"timestamp"`
			IsMeta      bool   `json:"isMeta"`
			IsSidechain bool   `json:"isSidechain"`
			Message     *struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		if e.IsMeta || e.IsSidechain || e.Message == nil || (e.Type != "user" && e.Type != "assistant") {
			continue
		}
		text := transcriptText(e.Message.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		if r := []rune(text); len(r) > maxTranscriptText {
			text = string(r[:maxTranscriptText]) + " …"
		}
		t.Messages = append(t.Messages, TranscriptMessage{Role: e.Message.Role, Text: text, At: e.Timestamp})
		t.UpdatedAt = e.Timestamp
	}
	if len(t.Messages) > maxTranscriptMessages {
		t.Messages = t.Messages[len(t.Messages)-maxTranscriptMessages:]
	}
	return t
}

// GetTranscripts returns the conversations of a session's launch cwd.
// found=false means no such session; an empty slice means none written yet.
func (m *Manager) GetTranscripts(id string) (transcripts []Transcript, found bool) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	var cwd, startedAt string
	if ok {
		cwd = s.cwd
		startedAt = s.StartedAt
	}
	m.mu.Unlock()
	if !ok {
		return nil, false
	}
	dir := filepath.Join(claudeProjectsDir, claudeProjectDirName(cwd))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []Transcript{}, true
	}
	started, startedErr := time.Parse(time.RFC3339, startedAt)
	out := []Transcript{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		t := parseTranscript(filepath.Join(dir, e.Name()))
		if t == nil || len(t.Messages) == 0 {
			continue
		}
		// same-dir sessions share their project dir with every past run in that
		// folder — keep only conversations that began after this session did
		// (worktree cwds are unique per launch, so everything passes)
		if startedErr == nil {
			if first, err := time.Parse(time.RFC3339, t.Messages[0].At); err == nil && first.Before(started.Add(-time.Minute)) {
				continue
			}
		}
		out = append(out, *t)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].UpdatedAt < out[b].UpdatedAt })
	return out, true
}

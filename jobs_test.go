package main

import (
	"strings"
	"testing"
)

func TestFirstRunes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		s    string
		n    int
		want string
	}{
		{"", 5, ""},
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 4, "hell"},
		{"héllö wörld", 5, "héllö"},
		{"🚀🚀🚀", 2, "🚀🚀"},
		{"日本語テスト", 3, "日本語"},
	}
	for _, tc := range cases {
		if got := firstRunes(tc.s, tc.n); got != tc.want {
			t.Errorf("firstRunes(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}

func TestPromptDigest(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", 100)
	hash, preview := promptDigest(long)
	if len(hash) != 12 {
		t.Errorf("hash length = %d, want 12", len(hash))
	}
	for _, c := range hash {
		if !strings.ContainsRune("0123456789abcdef", c) {
			t.Errorf("hash %q is not lowercase hex", hash)
			break
		}
	}
	if preview != strings.Repeat("é", 80) {
		t.Errorf("preview should be the first 80 runes, got %q", preview)
	}

	again, _ := promptDigest(long)
	if again != hash {
		t.Error("digest not deterministic")
	}
	other, _ := promptDigest("different prompt")
	if other == hash {
		t.Error("different prompts produced the same digest")
	}
}

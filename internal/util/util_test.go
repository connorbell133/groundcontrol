package util

import "testing"

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
		if got := FirstRunes(tc.s, tc.n); got != tc.want {
			t.Errorf("FirstRunes(%q, %d) = %q, want %q", tc.s, tc.n, got, tc.want)
		}
	}
}

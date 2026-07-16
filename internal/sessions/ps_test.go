package sessions

import (
	"reflect"
	"testing"
)

func TestParsePSOutput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want map[int]int
	}{
		{"empty", "", map[int]int{}},
		{"well-formed", "100 1\n200 100\n300 200\n", map[int]int{100: 1, 200: 100, 300: 200}},
		{"leading and trailing space", "  400   100  \n", map[int]int{400: 100}},
		{"torn line: one field", "100 1\n200\n300 200\n", map[int]int{100: 1, 300: 200}},
		{"torn line: three fields", "100 1 x\n200 100\n", map[int]int{200: 100}},
		{"non-integer fields skipped", "abc def\n200 100\n", map[int]int{200: 100}},
		{"blank lines tolerated", "\n\n100 1\n\n", map[int]int{100: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := parsePSOutput(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parsePSOutput(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

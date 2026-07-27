package actors

import "testing"

// TestExtractTabFlag covers the --tab flag parsing shared by ##pane list and
// ##tab list-panes.
func TestExtractTabFlag(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{"empty", nil, ""},
		{"no flag", []string{"foo", "bar"}, ""},
		{"long flag with value", []string{"--tab", "tab-123"}, "tab-123"},
		{"long flag equals form", []string{"--tab=tab-123"}, "tab-123"},
		{"short flag with value", []string{"-t", "tab-123"}, "tab-123"},
		{"flag among other args", []string{"foo", "--tab", "tab-123", "bar"}, "tab-123"},
		{"flag with no value", []string{"--tab"}, ""},
		{"first match wins", []string{"--tab", "a", "--tab", "b"}, "a"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := extractTabFlag(c.args); got != c.want {
				t.Errorf("extractTabFlag(%v) = %q, want %q", c.args, got, c.want)
			}
		})
	}
}

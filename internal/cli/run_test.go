package cli

import "testing"

// -n used to mean "do not follow", which read backwards against every other
// tool that spells a line count that way. --no-follow is the spelling now.
func TestParseLogArgs(t *testing.T) {
	for _, c := range []struct {
		name   string
		args   []string
		lines  int
		follow bool
	}{
		{"no arguments follow the default tail", nil, 50, true},
		{"a bare number is a line count", []string{"120"}, 120, true},
		{"-n takes the line count", []string{"-n", "120"}, 120, true},
		{"--lines is the long spelling", []string{"--lines", "7"}, 7, true},
		{"--no-follow stops at the tail", []string{"--no-follow"}, 50, false},
		{"both together", []string{"-n", "5", "--no-follow"}, 5, false},
		{"order does not matter", []string{"--no-follow", "-n", "5"}, 5, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			lines, follow, err := parseLogArgs(c.args)
			if err != nil {
				t.Fatalf("parseLogArgs(%q) failed: %v", c.args, err)
			}
			if lines != c.lines || follow != c.follow {
				t.Errorf("parseLogArgs(%q) = %d lines, follow=%v; want %d, %v",
					c.args, lines, follow, c.lines, c.follow)
			}
		})
	}
}

func TestParseLogArgsRejectsBadInput(t *testing.T) {
	// A typo has to fail loudly. Silently tailing the default would look like
	// the flag was honoured.
	for _, args := range [][]string{
		{"-n"},              // no value
		{"--lines"},         // no value
		{"-n", "many"},      // not a number
		{"-n", "-1"},        // negative short option
		{"--lines", "-1"},   // negative long option
		{"-1"},              // negative positional
		{"--not-a-flag"},    // unknown option
		{"--no-follow", ""}, // empty positional
	} {
		if _, _, err := parseLogArgs(args); err == nil {
			t.Errorf("parseLogArgs(%q) succeeded, want an error", args)
		}
	}
}

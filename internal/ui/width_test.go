package ui

import (
	"strings"
	"testing"
)

func TestCellsCountsColumnsNotRunes(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"plain ascii", 11},
		// Umlauts are one column each, as many runes as columns.
		{"für später", 10},
		// An emoji takes two columns although it is one rune, which is what
		// used to shift every column to its right by one.
		{"✨ Add feature", 14},
		// CJK is two columns per rune: seven runes, fourteen columns.
		{"日本語タイトル", 14},
		{"", 0},
	}
	for _, c := range cases {
		if got := Cells(c.in); got != c.want {
			t.Errorf("Cells(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestCellsIgnoresStyling(t *testing.T) {
	w := &Writer{Color: true, Links: true}
	// Styled and linked text has to measure as what the terminal shows, or a
	// column built out of already styled blocks is padded by escape bytes.
	if got := Cells(w.Bold("hello")); got != 5 {
		t.Errorf("Cells of bold text = %d, want 5", got)
	}
	if got := Cells(w.Link(w.Red("comment ↗"), "https://example.test/pr/42")); got != 9 {
		t.Errorf("Cells of a link = %d, want 9", got)
	}
	if got := Cells(w.Dim(w.Strike("acme/api #42"))); got != 12 {
		t.Errorf("Cells of nested styles = %d, want 12", got)
	}
}

func TestStripANSILeavesPlainTextAlone(t *testing.T) {
	if got := StripANSI("nothing to strip"); got != "nothing to strip" {
		t.Errorf("StripANSI changed plain text: %q", got)
	}
	// A truncated escape at the end must not be emitted as visible junk.
	if got := StripANSI("tail\x1b"); got != "tail" {
		t.Errorf("StripANSI(%q) = %q", "tail\\x1b", got)
	}
	if got := StripANSI("a\x1b]8;;http://x\x07b"); got != "ab" {
		t.Errorf("BEL terminated OSC not stripped: %q", got)
	}
}

func TestTruncateCutsOnColumnBoundaries(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 20, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"truncate me here", 8, "truncat…"},
		{"Jahrgangsstufenwechsel", 10, "Jahrgangs…"},
		{"anything", 0, ""},
		{"anything", 1, "…"},
		{"trailing space cut ", 15, "trailing space…"},
		// Six columns of budget is five plus the ellipsis, and three CJK
		// characters are six columns, so only two fit beside it.
		{"日本語タイトル", 6, "日本…"},
		// A double width rune must never be cut in half.
		{"日本語", 4, "日…"},
		{"Zeiterfassungs-Export für später", 12, "Zeiterfassu…"},
	}
	for _, c := range cases {
		got := Truncate(c.in, c.n)
		if got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
		if n := Cells(got); n > c.n {
			t.Errorf("Truncate(%q, %d) returned %d columns", c.in, c.n, n)
		}
	}
}

func TestPadMeasuresInColumns(t *testing.T) {
	if got := Pad("ab", 5); got != "ab   " {
		t.Errorf("Pad = %q", got)
	}
	if got := Pad("für", 5); got != "für  " {
		t.Errorf("Pad with umlaut = %q", got)
	}
	// Two CJK characters already fill four columns, so only one space is left.
	if got := Pad("日本", 5); got != "日本 " {
		t.Errorf("Pad with CJK = %q", got)
	}
	if got := Pad("too long already", 4); got != "too long already" {
		t.Errorf("Pad shortened a long value: %q", got)
	}
}

// The point of measuring in columns is that a column stays a column whatever
// the text in it is, which is what this asserts directly.
func TestPaddedTitlesAlignWhateverTheScript(t *testing.T) {
	for _, title := range []string{"plain", "für später", "✨ Add feature", "日本語タイトル"} {
		if got := Cells(Pad(Truncate(title, 20), 20)); got != 20 {
			t.Errorf("title %q occupies %d columns, want 20", title, got)
		}
	}
}

func TestRuleFillsTheBlockWidth(t *testing.T) {
	var b strings.Builder
	w := &Writer{Out: &b, Color: true, Width: 80}
	w.Rule()
	if got := Cells(strings.TrimRight(b.String(), "\n")); got != 80 {
		t.Errorf("rule on an 80 column terminal is %d columns", got)
	}
	// A very wide terminal is capped, so the rule matches the content beside it
	// instead of running to the screen edge.
	b.Reset()
	wide := &Writer{Out: &b, Color: true, Width: 400}
	wide.Rule()
	if got := Cells(strings.TrimRight(b.String(), "\n")); got != MaxWidth {
		t.Errorf("rule on a 400 column terminal is %d columns, want %d", got, MaxWidth)
	}
}

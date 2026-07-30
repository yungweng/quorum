package ui

import (
	"strings"
	"testing"
)

func TestColumnsAlignsRaggedBlocks(t *testing.T) {
	left := "REVIEWING\n  acme/api #42\n  toaster #7\n"
	right := "BABYSITTING\n  no fix loop\n"
	got := Columns(" | ", 0, left, right)
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("joined %d lines, want 3:\n%s", len(lines), got)
	}
	// The separator has to sit at the same offset on every row, including the
	// row where the short block has run out of content.
	want := strings.Index(lines[0], "|")
	for i, line := range lines {
		if got := strings.Index(line, "|"); got != want {
			t.Errorf("line %d puts the separator at %d, want %d: %q", i, got, want, line)
		}
	}
}

func TestColumnsMeasuresStyledAndWideText(t *testing.T) {
	w := &Writer{Color: true, Links: true}
	// A styled title and a CJK title in the same column: neither the escape
	// bytes nor the double width runes may move the separator.
	left := w.Bold("acme/api #42") + "\n" + "日本語\n"
	got := Columns(" | ", 0, left, "a\nb\n")
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	first := Cells(lines[0][:strings.Index(lines[0], "|")])
	for i, line := range lines {
		if got := Cells(line[:strings.Index(line, "|")]); got != first {
			t.Errorf("line %d has %d columns before the separator, want %d", i, got, first)
		}
	}
}

func TestColumnsLeavesNoTrailingWhitespace(t *testing.T) {
	got := Columns("  ", 0, "long line here\nshort\n", "a\n\n")
	for _, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if strings.HasSuffix(line, " ") {
			t.Errorf("line carries trailing whitespace: %q", line)
		}
	}
}

// A dashboard repaints every second. If the column width followed the longest
// line, the separator would slide sideways whenever a title changed, which
// reads as the whole screen twitching.
func TestColumnsHoldTheGivenWidthWhateverTheContent(t *testing.T) {
	at := func(block string) int {
		got := Columns("│", 40, block, "right\n")
		return strings.Index(strings.Split(got, "\n")[0], "│")
	}
	short := at("REVIEWING\n  idle\n")
	long := at("REVIEWING\n  acme/some-service #2051  a rather long title\n")
	if short != long {
		t.Errorf("separator moved from %d to %d when content grew", short, long)
	}
	if short != 40 {
		t.Errorf("separator at %d, want the requested width 40", short)
	}
}

// Sections open with a blank line to separate them from what came before. Side
// by side that would be a first row carrying nothing but the separator.
func TestColumnsDropLeadingBlankLines(t *testing.T) {
	got := Columns(" | ", 0, "\nREVIEWING\n  idle\n", "\nBABYSITTING\n  idle\n")
	if first := strings.Split(got, "\n")[0]; !strings.Contains(first, "REVIEWING") {
		t.Errorf("first row is not the headings: %q", first)
	}
}

func TestBlockRendersAgainstTheGivenWidth(t *testing.T) {
	w := &Writer{Color: true, Width: 200}
	got := w.Block(40, func(b *Writer) { b.Rule() })
	if n := Cells(strings.TrimRight(got, "\n")); n != 40 {
		t.Errorf("block rule is %d columns, want 40", n)
	}
	// Narrowing must not leak back into the writer it came from.
	if w.Cols() != MaxWidth {
		t.Errorf("parent writer width changed to %d", w.Cols())
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

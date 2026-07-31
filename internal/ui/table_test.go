package ui

import (
	"strings"
	"testing"
)

func cells(texts ...string) []Cell {
	out := make([]Cell, 0, len(texts))
	for _, text := range texts {
		out = append(out, Cell{Text: text})
	}
	return out
}

// columnOf is which screen column sub starts in, which is what a table is
// judged by and is neither the byte offset nor the rune offset.
func columnOf(line, sub string) int {
	plain := StripANSI(line)
	i := strings.Index(plain, sub)
	if i < 0 {
		return -1
	}
	return Cells(plain[:i])
}

// The whole point of the table: a short cell in one row does not pull the
// column after it to the left on that row only.
func TestColumnsPutTheSameColumnAtTheSameOffset(t *testing.T) {
	lines := Columns([][]Cell{
		cells("✓", "acme/very-long-repository #2074", "@example-user", "nothing found"),
		cells("✗", "acme/api #7", "", "failed"),
	}, 2, 200)

	a, b := columnOf(lines[0], "nothing found"), columnOf(lines[1], "failed")
	if a < 0 || b < 0 {
		t.Fatalf("the table lost a result:\n%s", strings.Join(lines, "\n"))
	}
	if a != b {
		t.Errorf("the result column starts at %d and at %d:\n%s", a, b, strings.Join(lines, "\n"))
	}
}

// Want is what lets two tables drawn separately share their offsets.
func TestColumnsHonourAWantedWidth(t *testing.T) {
	lines := Columns([][]Cell{
		{{Text: "-"}, {Text: "acme/api #7", Want: 30}, {Text: "nothing found"}},
	}, 2, 200)
	if got := columnOf(lines[0], "nothing found"); got != 1+2+30+2 {
		t.Errorf("the wanted width was not held: result column starts at %d in %q", got, lines[0])
	}
}

func TestColumnsNeverLeaveTrailingWhitespace(t *testing.T) {
	lines := Columns([][]Cell{
		cells("✓", "acme/api #7", "nothing found", "merged"),
		cells("✗", "acme/api #8", "failed", ""),
	}, 2, 200)
	for _, line := range lines {
		if line != strings.TrimRight(line, " ") {
			t.Errorf("line ends in whitespace: %q", line)
		}
	}
}

// Escape sequences take no columns, so a styled cell has to pad to the same
// width as a plain one. Getting this wrong is how one coloured line shifts a
// column for the whole table.
func TestColumnsMeasureStyledCellsByWhatIsShown(t *testing.T) {
	w := &Writer{Width: 200, Color: true}
	lines := Columns([][]Cell{
		{{Text: "acme/api #7", Style: func(s string) string { return w.Link(w.Bold(s), "https://example.test") }}, {Text: "nothing found"}},
		{{Text: "acme/api #8"}, {Text: "nothing found"}},
	}, 2, 200)
	if a, b := columnOf(lines[0], "nothing found"), columnOf(lines[1], "nothing found"); a != b {
		t.Errorf("a styled row put the result at %d and a plain one at %d:\n%q\n%q", a, b, lines[0], lines[1])
	}
}

// A narrow terminal costs the caller what the caller said it should cost, in
// the order it said, and never a wrapped line.
func TestColumnsSqueezeInGiveOrder(t *testing.T) {
	row := []Cell{
		{Text: "-"},
		{Text: "acme/api #7", Flex: true, Give: 2, Min: 8},
		{Text: "@example-user", Flex: true, Give: 0},
		{Text: "an explanation of what went wrong", Flex: true, Give: 1, Min: 10},
	}
	if got := Columns([][]Cell{row}, 2, 200)[0]; !strings.Contains(got, "@example-user") {
		t.Errorf("a table that fits lost a column: %q", got)
	}

	// The author gives way first, and whole: a login cut to three letters takes
	// room to say nothing.
	tight := Columns([][]Cell{row}, 2, 50)[0]
	if strings.Contains(tight, "@") {
		t.Errorf("the author was kept or stubbed instead of dropped: %q", tight)
	}
	if !strings.Contains(tight, "acme/api #7") {
		t.Errorf("the label gave way before the explanation: %q", tight)
	}
	if Cells(tight) > 50 {
		t.Errorf("the squeezed line is %d columns: %q", Cells(tight), tight)
	}

	// With nothing left to give, the line still fits rather than wrapping.
	narrow := Columns([][]Cell{row}, 2, 12)[0]
	if Cells(narrow) > 12 {
		t.Errorf("the narrow line is %d columns: %q", Cells(narrow), narrow)
	}
	if !strings.Contains(narrow, "acme") {
		t.Errorf("the label was dropped before the columns after it: %q", narrow)
	}
}

func TestColumnsHandleNoRows(t *testing.T) {
	if got := Columns(nil, 2, 80); got != nil {
		t.Errorf("Columns(nil) = %v", got)
	}
	if got := Columns([][]Cell{{}}, 2, 80); len(got) != 0 {
		t.Errorf("a table of empty rows produced %v", got)
	}
}

func TestColumnsDropAColumnThatIsEmptyEverywhere(t *testing.T) {
	lines := Columns([][]Cell{
		cells("-", "", "nothing found"),
		cells("-", "", "failed"),
	}, 2, 200)
	if got := columnOf(lines[0], "nothing found"); got != 3 {
		t.Errorf("an empty column still took room: result column starts at %d in %q", got, lines[0])
	}
}

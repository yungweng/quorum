package ui

import (
	"slices"
	"strings"
)

// Cell is one column of one row.
//
// Text is kept plain and Style is applied afterwards, because fitting a column
// means cutting its text and cutting a string that already carries escape
// sequences produces broken ones. That is the same order every other caller in
// this package uses: truncate, then style.
//
// Flex, Give and Min say how far the column may be squeezed when the row does
// not fit the terminal. A column that is not Flex keeps its natural width and
// pushes the ones that are. Give is the order they give way in, lowest first,
// so the caller decides what a narrow terminal costs rather than the layout
// deciding it from where the columns happen to sit.
//
// Min is the narrowest a flexible column may become before the squeeze moves on
// to the next one. It is a floor, never a floor that widens, so a column whose
// content is already narrower than Min stays that narrow. A Min of zero means
// something different in kind: that column is all or nothing, and is dropped
// whole rather than shrunk. Use it for a column whose content is only worth
// anything entire, such as a login, where three letters and an ellipsis take up
// space to tell the reader nothing.
//
// Want widens a column past what this table's own content needs, which is how
// two tables drawn separately end up with the same offsets. Without it every
// block on a screen measures only itself, and the one column a reader follows
// down the whole screen steps sideways at each heading.
// Cut is how the column is shortened when it does not get the width it asked
// for. The default drops the tail, which is right for a sentence and wrong for
// anything whose meaning sits at the end: a pull request is looked up by the
// number its label ends in, and a label cut from the right keeps the part that
// says the least.
type Cell struct {
	Text  string
	Style func(string) string
	Cut   func(string, int) string
	Want  int
	Flex  bool
	Give  int
	Min   int
}

// Columns lays rows of cells out as a table, each column as wide as its widest
// cell, and returns one rendered line per row.
//
// This is what makes a list of runs readable as a list: with every line free to
// choose its own offsets, finding the result of a run means finding the result
// column again on every line, and two lines that report the same thing look
// like they report different things. Measuring across the rows first is also the
// only way to get it right, because how wide the widest repository name is, is
// not knowable while the first line is being written.
//
// gap is the number of spaces between columns. cols is the terminal width the
// table has to fit into: the flexible columns are squeezed in Give order first,
// and only once every one of them is at its floor are trailing columns dropped
// outright. Trailing empty cells are not padded, so no line carries trailing
// whitespace.
func Columns(rows [][]Cell, gap, cols int) []string {
	n := 0
	for _, row := range rows {
		n = max(n, len(row))
	}
	if n == 0 {
		return nil
	}

	width, floor, give := make([]int, n), make([]int, n), make([]int, n)
	flex := make([]bool, n)
	for _, row := range rows {
		for i, c := range row {
			width[i] = max(width[i], Cells(c.Text), c.Want)
			if c.Flex {
				flex[i], floor[i], give[i] = true, max(floor[i], c.Min), c.Give
			}
		}
	}
	for i := range floor {
		if !flex[i] {
			floor[i] = width[i]
			continue
		}
		floor[i] = min(floor[i], width[i])
	}
	fitColumns(width, floor, give, flex, gap, cols)

	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, renderRow(row, width, gap))
	}
	return lines
}

// fitColumns narrows width in place until the table fits cols.
//
// Each flexible column is spent entirely, first shrunk to its Min and then
// dropped outright, before the next one in Give order loses a single cell. That
// is what Min is for: it is the width below which the column stops being worth
// its room, so a column that cannot have it is better gone than kept as a
// prefix and an ellipsis. Sharing the squeeze out instead leaves a table where
// nothing was dropped and nothing can be read either, which is the worse of the
// two: a column that is gone is understood as gone, while one cut to four cells
// reads as content right up until it is looked at.
func fitColumns(width, floor, give []int, flex []bool, gap, cols int) {
	order := make([]int, 0, len(width))
	for i := range width {
		if flex[i] {
			order = append(order, i)
		}
	}
	slices.SortStableFunc(order, func(a, b int) int { return give[a] - give[b] })
	for _, i := range order {
		if tableWidth(width, gap) <= cols {
			return
		}
		// A Min of zero is the all or nothing case, so the shrink is skipped
		// entirely and the column goes whole. Shrinking it would stop at
		// whatever width happened to fit, which for a login is three letters
		// and an ellipsis: room spent to say nothing.
		for floor[i] > 0 && width[i] > floor[i] && tableWidth(width, gap) > cols {
			width[i]--
		}
		if tableWidth(width, gap) > cols {
			width[i] = 0
		}
	}
	// Nothing flexible is left, so the only room still to be had is in the
	// columns that never offered any. They go from the right, which is the end a
	// table stops being read at.
	for i := len(width) - 1; i >= 0 && tableWidth(width, gap) > cols; i-- {
		width[i] = 0
	}
}

// tableWidth is what the laid out table occupies. A column of width zero holds
// nothing anywhere in the table, so it costs neither its width nor a gap.
func tableWidth(width []int, gap int) int {
	total, shown := 0, 0
	for _, w := range width {
		if w > 0 {
			total, shown = total+w, shown+1
		}
	}
	if shown > 1 {
		total += gap * (shown - 1)
	}
	return total
}

func renderRow(row []Cell, width []int, gap int) string {
	last := -1
	for i := range width {
		if width[i] > 0 && i < len(row) && row[i].Text != "" {
			last = i
		}
	}
	var b strings.Builder
	for i := 0; i <= last; i++ {
		if width[i] == 0 {
			continue
		}
		text := ""
		if i < len(row) {
			text = Truncate(row[i].Text, width[i])
			if cut := row[i].Cut; cut != nil {
				text = Truncate(cut(row[i].Text, width[i]), width[i])
			}
		}
		styled := text
		if text != "" && i < len(row) && row[i].Style != nil {
			styled = row[i].Style(text)
		}
		if b.Len() > 0 {
			b.WriteString(strings.Repeat(" ", gap))
		}
		if i == last {
			b.WriteString(styled)
			break
		}
		b.WriteString(Pad(styled, width[i]))
	}
	return b.String()
}

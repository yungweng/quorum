package ui

import (
	"bytes"
	"strings"
)

// Narrow returns a copy of w that lays out against a smaller width, used to
// render a block that will end up inside a column.
func (w *Writer) Narrow(width int) *Writer {
	c := *w
	c.Width = width
	return &c
}

// Block renders into a string using w's terminal capabilities, which is how a
// section becomes something that can be measured and placed rather than
// something that has already reached the screen.
func (w *Writer) Block(width int, draw func(*Writer)) string {
	var b bytes.Buffer
	draw(w.Narrow(width).To(&b))
	return b.String()
}

// Columns joins already rendered blocks side by side, separated by sep.
//
// Blocks are ragged: one section may list four pull requests while the one
// beside it lists none. Short blocks are padded so the separator stays in one
// straight line down the frame, and the joined lines are trimmed on the right
// so no frame carries invisible trailing whitespace into a pipe or a test.
//
// width is the column width to hold every block to. Without it a column would
// be only as wide as its own longest line, which on a dashboard that repaints
// every second means the separator moves sideways whenever a pull request
// title changes. Pass 0 to let the content decide.
//
// Leading blank lines are dropped from every block. Sections begin with one to
// separate them from what came before, and side by side that turns into a row
// carrying nothing but the separator.
func Columns(sep string, width int, blocks ...string) string {
	if len(blocks) == 0 {
		return ""
	}
	split := make([][]string, len(blocks))
	widths := make([]int, len(blocks))
	height := 0
	for i, block := range blocks {
		trimmed := strings.TrimRight(strings.TrimLeft(block, "\n"), "\n")
		split[i] = strings.Split(trimmed, "\n")
		height = max(height, len(split[i]))
		if width > 0 {
			// A fixed width, not a minimum. One line that overruns its column
			// then pushes only its own row out of line, where widening the
			// whole column would move the separator on every row instead.
			widths[i] = width
			continue
		}
		for _, line := range split[i] {
			widths[i] = max(widths[i], Cells(line))
		}
	}

	var b strings.Builder
	for row := range height {
		var line strings.Builder
		for i, lines := range split {
			if i > 0 {
				line.WriteString(sep)
			}
			text := ""
			if row < len(lines) {
				text = lines[row]
			}
			// The last column needs no padding: nothing follows it.
			if i == len(split)-1 {
				line.WriteString(text)
			} else {
				line.WriteString(Pad(text, widths[i]))
			}
		}
		b.WriteString(strings.TrimRight(line.String(), " "))
		b.WriteString("\n")
	}
	return b.String()
}

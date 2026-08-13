package ui

import "strings"

// Box draws lines inside a single-line frame with the title set into the top
// border, the way full-screen dashboards title their panels:
//
//	┌ Open · 5 ──────────────┐
//	│ content                │
//	└────────────────────────┘
//
// style colours the frame, and the title is bold on top of the same colour, so
// a panel can be found by name and by hue. Box never truncates content: each
// line is padded to the inner width, which is Cols()-4 (a border and a space
// of padding on either side), so callers lay their content out against that
// width to begin with. It is only called on a colour terminal; piped output
// keeps plain headings instead of frames.
func (w *Writer) Box(title string, style func(string) string, lines []string) {
	cols := max(w.Cols(), 8)
	inner := cols - 4
	t := Truncate(title, inner)
	run := max(cols-4-Cells(t), 0)
	w.Println(style("┌ ") + w.Bold(style(t)) + style(" "+strings.Repeat("─", run)+"┐"))
	for _, line := range lines {
		w.Println(style("│") + " " + Pad(line, inner) + " " + style("│"))
	}
	w.Println(style("└" + strings.Repeat("─", cols-2) + "┘"))
}

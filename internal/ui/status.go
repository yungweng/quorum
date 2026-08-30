package ui

import (
	"fmt"
	"strings"
	"time"
)

// spinFrames is the braille spinner both shell tools used.
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Symbols degrade to ASCII without a terminal, so a piped log stays readable.
func (w *Writer) SymOK() string {
	if w.Color {
		return "✓"
	}
	return "ok:"
}

func (w *Writer) SymFail() string {
	if w.Color {
		return "✗"
	}
	return "FAIL:"
}

// SymWarn marks a warning or a gate that needs a decision.
func (w *Writer) SymWarn() string {
	if w.Color {
		return "⚠"
	}
	return "warn:"
}

// SymSkip marks work that finished but was superseded, like a review of a
// head that a CI fix moved on from.
func (w *Writer) SymSkip() string {
	if w.Color {
		return "↻"
	}
	return "--"
}

// Rule prints a horizontal separator across the block width.
//
// It used to be sixty columns whatever the terminal did, which on a wide
// screen stops halfway under content that keeps going and is the single most
// obvious sign of output that was never fitted to its terminal.
func (w *Writer) Rule() {
	unit := "-"
	if w.Color {
		unit = "─"
	}
	fmt.Fprintln(w.Out, w.Dim(strings.Repeat(unit, max(w.Cols(), 1))))
}

// labelColumn is the width of the label column in a run header. It is wide
// enough for the longest label any header uses, so review and babysit line
// their values up at the same offset.
const labelColumn = 12

// Row prints one aligned "label   value" line of a run header. Labels are dim
// and lowercase so the eye lands on the values, which are what change.
func (w *Writer) Row(label, value string) {
	fmt.Fprintf(w.Out, "  %s %s\n", w.Dim(Pad(strings.ToLower(label), labelColumn)), value)
}

// Step prints a section heading with a blank line before it, in the same voice
// as Section so a babysit run and the dashboard read as one tool. On a
// terminal a dim rule fills the rest of the line, so the phases of a long run
// are findable while scrolling; piped output keeps the bare heading.
func (w *Writer) Step(title string) {
	fmt.Fprintln(w.Out)
	head := strings.ToUpper(title)
	if !w.Color {
		fmt.Fprintln(w.Out, head)
		return
	}
	fill := w.Cols() - Cells(head) - 1
	if fill > 0 {
		head = w.Bold(head) + " " + w.Dim(strings.Repeat("─", fill))
	} else {
		head = w.Bold(head)
	}
	fmt.Fprintln(w.Out, head)
}

// Status is the single transient line at the bottom of the output, redrawn in
// place. Everything permanent goes through the normal print methods, so piping
// the output to a file produces no spinner residue at all.
type Status struct {
	w     *Writer
	shown bool
	tick  int
}

func (w *Writer) Status() *Status { return &Status{w: w} }

// Draw replaces the status line with text.
func (s *Status) Draw(text string) {
	if !s.w.Color {
		return
	}
	fmt.Fprintf(s.w.Out, "\r\x1b[K%s", s.w.Dim(text))
	s.shown = true
}

// Spin draws a label with the next spinner frame and an elapsed time.
func (s *Status) Spin(label string, elapsed time.Duration) {
	frame := spinFrames[s.tick%len(spinFrames)]
	s.tick++
	s.Draw(fmt.Sprintf("%s %s · %s", frame, label, Duration(elapsed)))
}

// Clear removes the status line, so the next permanent line starts clean.
func (s *Status) Clear() {
	if !s.shown {
		return
	}
	fmt.Fprint(s.w.Out, "\r\x1b[K")
	s.shown = false
}

// Spinner returns the current frame of the braille spinner for tick.
func Spinner(tick int) string { return spinFrames[tick%len(spinFrames)] }

// Bar draws a proportional progress bar n columns wide.
//
// The filled and empty halves use different characters rather than only
// different colours, so the bar still reads as a bar where colour is off.
func (w *Writer) Bar(done, total, n int) string {
	if total <= 0 || n <= 0 {
		return ""
	}
	filled := done * n / total
	filled = min(max(filled, 0), n)
	return w.Cyan(strings.Repeat("█", filled)) + w.Dim(strings.Repeat("░", n-filled))
}

// Panel is a block of lines redrawn in place, the multi line counterpart of
// Status. It is what lets a review show one line per reviewer that updates as
// they finish, instead of collapsing six parallel processes into one counter.
//
// Without a terminal it draws nothing at all, exactly like Status, so a piped
// run leaves no cursor movement in the log and reports through its permanent
// output instead.
type Panel struct {
	w     *Writer
	lines int
}

func (w *Writer) Panel() *Panel { return &Panel{w: w} }

// Draw replaces the panel's previous content with lines.
//
// Movement is relative to where the cursor is, so a panel near the bottom of
// the screen survives the terminal scrolling under it. Each line erases its own
// tail, and one erase at the end removes rows a previous, taller frame left
// behind.
func (p *Panel) Draw(lines []string) {
	if !p.w.Color {
		return
	}
	var b strings.Builder
	if p.lines > 0 {
		fmt.Fprintf(&b, "\x1b[%dF", p.lines)
	}
	for _, line := range lines {
		b.WriteString("\r")
		b.WriteString(line)
		b.WriteString("\x1b[K\n")
	}
	b.WriteString("\x1b[J")
	fmt.Fprint(p.w.Out, b.String())
	p.lines = len(lines)
}

// Freeze stops redrawing and leaves the last frame on the screen, which is how
// the finished reviewer list stays in the scrollback.
func (p *Panel) Freeze() { p.lines = 0 }

// Clear removes the panel entirely.
func (p *Panel) Clear() {
	if !p.w.Color || p.lines == 0 {
		return
	}
	fmt.Fprintf(p.w.Out, "\x1b[%dF\x1b[J", p.lines)
	p.lines = 0
}

// Notify sends an OSC 777 terminal notification.
//
// Writing the escape sequence directly is what avoids making a terminal's own
// CLI a runtime dependency. cmux and other terminals with notification support
// understand it; the rest ignore it.
func (w *Writer) Notify(title, body string) {
	if !w.Color {
		return
	}
	fmt.Fprintf(w.Out, "\x1b]777;notify;%s;%s\x07", sanitize(title), sanitize(body))
}

// Bell rings the terminal bell, which works even where OSC 777 does not.
func (w *Writer) Bell() {
	if w.Color {
		fmt.Fprint(w.Out, "\a")
	}
}

// sanitize strips what would break out of, or truncate, an OSC 777 sequence:
// the escape and bell characters that terminate it, the newlines that a
// terminal would not render anyway, and the semicolons that separate its
// fields. A finding title quoted from a PR can contain all of them.
func sanitize(s string) string {
	return strings.NewReplacer(
		"\x1b", "",
		"\a", "",
		"\n", " ",
		"\r", " ",
		";", ",",
	).Replace(s)
}

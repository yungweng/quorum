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

// Rule prints a horizontal separator.
func (w *Writer) Rule() {
	line := strings.Repeat("-", 60)
	if w.Color {
		line = strings.Repeat("─", 60)
	}
	fmt.Fprintln(w.Out, w.Dim(line))
}

// Row prints one aligned "label   value" line of a run header.
func (w *Writer) Row(label, value string) {
	fmt.Fprintf(w.Out, "  %-10s %s\n", label, value)
}

// Step prints a section heading with a blank line before it.
func (w *Writer) Step(title string) {
	fmt.Fprintln(w.Out)
	fmt.Fprintln(w.Out, w.Bold("==> "+title))
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

// Package ui renders prbot's terminal output.
//
// Everything degrades: without a terminal there are no colours, no hyperlinks
// and no box drawing, so piping status into a file or a launchd log produces
// plain readable text.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/term"
)

// Writer renders to one output stream.
type Writer struct {
	Out    io.Writer
	Color  bool
	Links  bool
	Width  int
	indent string
}

// New inspects out and the environment to decide what the terminal can do.
// NO_COLOR turns styling off, as does anything that is not a terminal.
func New(out *os.File) *Writer {
	w := &Writer{Out: out, Width: 100}
	fd := int(out.Fd())
	if !term.IsTerminal(fd) {
		return w
	}
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return w
	}
	w.Color = true
	// OSC 8 hyperlinks are ignored by terminals that do not know them, but a
	// few older ones print the escape instead, so keep it to a terminal that
	// announces itself.
	w.Links = true
	if width, _, err := term.GetSize(fd); err == nil && width > 20 {
		w.Width = width
	}
	return w
}

// ANSI attributes.
const (
	reset     = "\x1b[0m"
	bold      = "\x1b[1m"
	dim       = "\x1b[2m"
	fgRed     = "\x1b[31m"
	fgGreen   = "\x1b[32m"
	fgYellow  = "\x1b[33m"
	fgBlue    = "\x1b[34m"
	fgMagenta = "\x1b[35m"
	fgCyan    = "\x1b[36m"
)

func (w *Writer) style(code, s string) string {
	if !w.Color || s == "" {
		return s
	}
	return code + s + reset
}

func (w *Writer) Bold(s string) string    { return w.style(bold, s) }
func (w *Writer) Dim(s string) string     { return w.style(dim, s) }
func (w *Writer) Red(s string) string     { return w.style(fgRed, s) }
func (w *Writer) Green(s string) string   { return w.style(fgGreen, s) }
func (w *Writer) Yellow(s string) string  { return w.style(fgYellow, s) }
func (w *Writer) Blue(s string) string    { return w.style(fgBlue, s) }
func (w *Writer) Magenta(s string) string { return w.style(fgMagenta, s) }
func (w *Writer) Cyan(s string) string    { return w.style(fgCyan, s) }

// Link wraps text in an OSC 8 hyperlink so clicking it opens url. Terminals
// without support print text alone, which is why text must stand on its own.
func (w *Writer) Link(text, url string) string {
	if !w.Links || url == "" {
		return text
	}
	return "\x1b]8;;" + url + "\x1b\\" + text + "\x1b]8;;\x1b\\"
}

func (w *Writer) Printf(format string, args ...any) {
	fmt.Fprintf(w.Out, w.indent+format, args...)
}

func (w *Writer) Println(s string) {
	fmt.Fprintln(w.Out, w.indent+s)
}

// Section prints a heading with a blank line before it.
func (w *Writer) Section(title string, count int, total int) {
	fmt.Fprintln(w.Out)
	head := strings.ToUpper(title)
	switch {
	case total > 0:
		head = fmt.Sprintf("%s  %d of %d", head, count, total)
	case count > 0:
		head = fmt.Sprintf("%s  %d", head, count)
	}
	fmt.Fprintln(w.Out, w.Bold(head))
}

// Truncate shortens s to at most n display cells, marking the cut with an
// ellipsis. It counts runes, which is right for the titles prbot prints.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	runes := []rune(s)
	return strings.TrimRight(string(runes[:n-1]), " ") + "…"
}

// Pad right-pads s with spaces to n cells. A string already at or over the
// width is returned untouched, so a long field pushes the line instead of
// being silently cut.
func Pad(s string, n int) string {
	if d := n - utf8.RuneCountInString(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

// Ago renders how long ago t was, in the shortest form that stays unambiguous.
func Ago(t time.Time) string { return AgoFrom(time.Now(), t) }

// AgoFrom is Ago with an explicit now, so it can be tested.
func AgoFrom(now, t time.Time) string {
	if t.IsZero() {
		return ""
	}
	d := now.Sub(t)
	if d < 0 {
		return "just now"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("2 Jan")
	}
}

// Duration renders an elapsed time as "6m" or "1h12m".
func Duration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// Bytes renders a size the way a person reads it.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 3 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}

// Screen control, used by watch. All are no-ops without a terminal.
func (w *Writer) AltScreen(on bool) {
	if !w.Color {
		return
	}
	if on {
		fmt.Fprint(w.Out, "\x1b[?1049h\x1b[?25l")
	} else {
		fmt.Fprint(w.Out, "\x1b[?25h\x1b[?1049l")
	}
}

func (w *Writer) Home() {
	if w.Color {
		fmt.Fprint(w.Out, "\x1b[H\x1b[2J")
	}
}

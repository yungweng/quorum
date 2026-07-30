// Package ui renders quorum's terminal output.
//
// Everything degrades: without a terminal there are no colours, no hyperlinks
// and no box drawing, so piping status into a file or a launchd log produces
// plain readable text.
package ui

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"
)

// Writer renders to one output stream.
type Writer struct {
	Out   io.Writer
	Color bool
	Links bool
	Width int
	// Height is the terminal's row count, 0 when it is not a terminal. Only
	// watch uses it, to keep a frame from scrolling the screen it is painting.
	Height int
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
	if width, height, err := term.GetSize(fd); err == nil && width > 20 {
		w.Width = width
		w.Height = height
	}
	return w
}

// MaxWidth caps how wide a block of output grows: two classic eighty column
// blocks, which is the widest layout the dashboard actually builds.
//
// Terminals are routinely wider than this, and a rule or a right aligned field
// that runs to the edge of a very wide screen reads as a mistake rather than a
// decision. Everything that measures itself uses Cols rather than Width, so
// rules, columns and right aligned fields all end at the same place.
const MaxWidth = 160

// Cols is the width layout is built against.
func (w *Writer) Cols() int {
	if w.Width < MaxWidth {
		return w.Width
	}
	return MaxWidth
}

// To returns a copy of w that renders somewhere else while keeping the
// terminal's capabilities. watch builds a whole frame in memory this way, which
// is what lets it reach the screen in a single write.
func (w *Writer) To(out io.Writer) *Writer {
	c := *w
	c.Out = out
	return &c
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
	strike    = "\x1b[9m"
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

// Strike crosses text out. Terminals that do not know SGR 9 print the text
// plainly, so nothing may rely on it alone to carry meaning.
func (w *Writer) Strike(s string) string { return w.style(strike, s) }

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

// Section prints a heading with a blank line before it, and its count beside
// it.
//
// The count sits next to the title rather than out at the right edge. Pushed
// to the edge it lines up with the counts of other sections, which is worth
// something in a grid of them and nothing at all in a single column: on a wide
// terminal it leaves a hundred columns of nothing between a word and a number
// that belong together.
func (w *Writer) Section(title string, count int, total int) {
	fmt.Fprintln(w.Out)
	head := w.Bold(strings.ToUpper(title))
	switch {
	case total > 0:
		head += w.Dim(fmt.Sprintf("  %d / %d", count, total))
	case count > 0:
		head += w.Dim("  " + strconv.Itoa(count))
	}
	fmt.Fprintln(w.Out, head)
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

// Paint puts one prepared frame on the screen.
//
// It deliberately does not clear first. Erasing the screen and then drawing
// into it leaves a moment where the terminal has nothing to show, and at a
// redraw every few seconds that moment is the flicker. Instead the cursor goes
// home, every line erases only its own tail as it is written, and one erase at
// the end removes whatever the previous frame left below this one. The whole
// frame reaches the terminal in a single write, so no partial screen is ever
// visible either.
func (w *Writer) Paint(frame string) {
	if !w.Color {
		fmt.Fprint(w.Out, frame)
		return
	}
	lines := strings.Split(strings.TrimRight(frame, "\n"), "\n")
	// A frame taller than the screen scrolls it, and once it has scrolled the
	// next frame's cursor-home lands in the wrong place, which leaves exactly
	// the debris this is meant to avoid. Cutting whole lines keeps every escape
	// sequence balanced. A line wider than the terminal still wraps and counts
	// as one, so callers keep their lines inside Width.
	if w.Height > 0 && len(lines) > w.Height {
		lines = lines[:w.Height]
	}
	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, line := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(line)
		b.WriteString("\x1b[K")
	}
	b.WriteString("\x1b[J")
	fmt.Fprint(w.Out, b.String())
}

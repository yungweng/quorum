package ui

import (
	"strings"

	"github.com/mattn/go-runewidth"
)

// Cells is how many terminal columns s occupies.
//
// This is neither len nor the rune count, and the difference is not academic:
// an emoji or a CJK character takes two columns, a combining mark takes none,
// and the escape sequences this package emits take none at all. A pull request
// titled "✨ Add feature" is thirteen runes and fourteen columns, and a
// Japanese title is half as many runes as columns. Measuring either of them in
// runes shifts every column to its right, which is how one title breaks the
// alignment of a whole dashboard.
//
// Escapes are stripped before measuring so that already styled text can be
// padded into a column. Only the sequences this package produces need to be
// recognised, because it produces all of them: SGR attributes from style and
// OSC 8 hyperlinks from Link.
func Cells(s string) int {
	return runewidth.StringWidth(StripANSI(s))
}

// StripANSI removes the escape sequences from s, leaving what a terminal would
// actually show.
func StripANSI(s string) string {
	if !strings.ContainsRune(s, 0x1b) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != 0x1b {
			b.WriteByte(s[i])
			i++
			continue
		}
		i = skipEscape(s, i)
	}
	return b.String()
}

// skipEscape returns the index just past the escape sequence starting at i.
func skipEscape(s string, i int) int {
	// A lone ESC at the end of the string introduces nothing.
	if i+1 >= len(s) {
		return len(s)
	}
	switch s[i+1] {
	case '[':
		// CSI: parameters and intermediates, then one final byte in @ to ~.
		for j := i + 2; j < len(s); j++ {
			if s[j] >= '@' && s[j] <= '~' {
				return j + 1
			}
		}
		return len(s)
	case ']':
		// OSC: runs until BEL or the two byte string terminator ESC backslash.
		for j := i + 2; j < len(s); j++ {
			if s[j] == 0x07 {
				return j + 1
			}
			if s[j] == 0x1b && j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
		}
		return len(s)
	default:
		// Any other two byte escape, which includes the ESC backslash that
		// terminates an OSC when it is reached on its own.
		return i + 2
	}
}

// Truncate shortens s to at most n display columns, marking the cut with an
// ellipsis.
//
// It expects unstyled text. Cutting a string in the middle of an escape
// sequence would emit a broken one, so callers truncate first and style the
// result, which is the order every caller in quorum uses.
func Truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if Cells(s) <= n {
		return s
	}
	if n == 1 {
		return "…"
	}
	// One column is reserved for the ellipsis. A double width rune that would
	// straddle the limit is dropped whole rather than half printed.
	room, used := n-1, 0
	var b strings.Builder
	for _, r := range s {
		wide := runewidth.RuneWidth(r)
		if used+wide > room {
			break
		}
		used += wide
		b.WriteRune(r)
	}
	return strings.TrimRight(b.String(), " ") + "…"
}

// Pad right-pads s with spaces to n display columns. A string already at or
// over the width is returned untouched, so a long field pushes the line
// instead of being silently cut.
func Pad(s string, n int) string {
	if d := n - Cells(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

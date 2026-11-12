// Package firstline shortens command output to the one line an error message
// has room for.
package firstline

import "strings"

// Trimmed returns s up to its first newline, without surrounding space.
func Trimmed(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return strings.TrimSpace(line)
}

// OrNoOutput returns the first line of s once s is trimmed, or "no output"
// when nothing is left.
func OrNoOutput(s string) string {
	s = strings.TrimSpace(s)
	if line, _, ok := strings.Cut(s, "\n"); ok {
		return line
	}
	if s == "" {
		return "no output"
	}
	return s
}

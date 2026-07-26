package ui

import (
	"strings"
	"testing"
	"time"
)

func TestTruncate(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"short", 20, "short"},
		{"exactly ten", 11, "exactly ten"},
		{"truncate me here", 8, "truncat…"},
		{"Jahrgangsstufenwechsel", 10, "Jahrgangs…"},
		{"trailing space cut ", 15, "trailing space…"},
		{"anything", 0, ""},
		{"anything", 1, "…"},
		// Umlauts are one rune each and must not be cut in half.
		{"Zeiterfassungs-Export für später", 12, "Zeiterfassu…"},
	}
	for _, c := range cases {
		got := Truncate(c.in, c.n)
		if got != c.want {
			t.Errorf("Truncate(%q, %d) = %q, want %q", c.in, c.n, got, c.want)
		}
		if n := len([]rune(got)); n > c.n {
			t.Errorf("Truncate(%q, %d) returned %d runes", c.in, c.n, n)
		}
	}
}

func TestPad(t *testing.T) {
	if got := Pad("ab", 5); got != "ab   " {
		t.Errorf("Pad = %q", got)
	}
	// Umlauts must count as one cell, not as their byte length.
	if got := Pad("für", 5); got != "für  " {
		t.Errorf("Pad with umlaut = %q", got)
	}
	if got := Pad("too long already", 4); got != "too long already" {
		t.Errorf("Pad shortened a long value: %q", got)
	}
}

func TestAgoFrom(t *testing.T) {
	now := time.Date(2026, 7, 26, 23, 0, 0, 0, time.UTC)
	cases := []struct {
		ago  time.Duration
		want string
	}{
		{10 * time.Second, "just now"},
		{6 * time.Minute, "6m ago"},
		{59 * time.Minute, "59m ago"},
		{3 * time.Hour, "3h ago"},
		{26 * time.Hour, "yesterday"},
		{3 * 24 * time.Hour, "3d ago"},
		{30 * 24 * time.Hour, "26 Jun"},
	}
	for _, c := range cases {
		if got := AgoFrom(now, now.Add(-c.ago)); got != c.want {
			t.Errorf("AgoFrom(-%s) = %q, want %q", c.ago, got, c.want)
		}
	}
	if got := AgoFrom(now, time.Time{}); got != "" {
		t.Errorf("zero time rendered as %q", got)
	}
	// Clock skew must not produce "-3m ago".
	if got := AgoFrom(now, now.Add(time.Minute)); got != "just now" {
		t.Errorf("future time rendered as %q", got)
	}
}

func TestDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{6 * time.Minute, "6m"},
		{72 * time.Minute, "1h12m"},
	}
	for _, c := range cases {
		if got := Duration(c.d); got != c.want {
			t.Errorf("Duration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{512, "512 B"},
		{1536, "1.5 KB"},
		{1258291200, "1.2 GB"},
	}
	for _, c := range cases {
		if got := Bytes(c.n); got != c.want {
			t.Errorf("Bytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

// Without a terminal nothing may emit escape sequences, or logs and pipes fill
// up with control characters.
func TestPlainWriterEmitsNoEscapes(t *testing.T) {
	var b strings.Builder
	w := &Writer{Out: &b, Width: 80}
	line := w.Bold("bold") + w.Red("red") + w.Link("title", "https://example.com")
	if strings.Contains(line, "\x1b") {
		t.Errorf("escape sequence leaked into plain output: %q", line)
	}
	if line != "boldredtitle" {
		t.Errorf("plain output = %q", line)
	}
	w.AltScreen(true)
	w.Home()
	if strings.Contains(b.String(), "\x1b") {
		t.Errorf("screen control leaked into plain output: %q", b.String())
	}
}

func TestLinkWrapsTextWhenSupported(t *testing.T) {
	var b strings.Builder
	w := &Writer{Out: &b, Color: true, Links: true, Width: 80}
	got := w.Link("#2017", "https://github.com/acme/api/pull/2017")
	want := "\x1b]8;;https://github.com/acme/api/pull/2017\x1b\\#2017\x1b]8;;\x1b\\"
	if got != want {
		t.Errorf("Link = %q, want %q", got, want)
	}
	// An empty URL must not produce a link with nowhere to go.
	if got := w.Link("#2017", ""); got != "#2017" {
		t.Errorf("empty url produced %q", got)
	}
}

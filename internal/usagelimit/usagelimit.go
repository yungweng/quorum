// Package usagelimit is the shared shape of one failure class: the engine's
// account has no quota left, so every further invocation is doomed until the
// provider resets it. Both engine adapters produce this error and the agent,
// the fix loop and the CLI all branch on it, which is why it lives below all
// of them instead of inside one adapter.
package usagelimit

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Err is the sentinel every usage-limit failure wraps. Test with errors.Is;
// recover the reset time with errors.As and a *Error.
var Err = errors.New("usage limit reached")

// Error reports that an engine refused to run because the account's usage
// limit was reached. A message quorum could not parse still produces this
// type with a zero ResetAt: the run is a usage-limit failure either way,
// callers just don't know when to retry.
type Error struct {
	ResetAt time.Time
	Line    string // the matched output line, for logging
}

func (e *Error) Error() string {
	if e.ResetAt.IsZero() {
		return "usage limit reached"
	}
	return fmt.Sprintf("usage limit reached, resets %s", e.ResetAt.Format("Mon, 02 Jan 2006 15:04"))
}

func (e *Error) Unwrap() error { return Err }

var (
	tryAgainAt    = regexp.MustCompile(`(?i)try again at\s+(.+?)\.?\s*$`)
	ordinalSuffix = regexp.MustCompile(`\b(\d{1,2})(st|nd|rd|th)\b`)
)

// ParseResetAt reads a "try again at Aug 10th, 2026 7:32 PM" clause out of
// line, tolerant of the ordinal day suffix and meridiem case. The message
// carries no timezone, so the result is interpreted in local time: an
// approximation good enough to schedule a retry, not an authoritative
// deadline. A line without a parsable clause yields the zero time.
func ParseResetAt(line string) time.Time {
	m := tryAgainAt.FindStringSubmatch(line)
	if m == nil {
		return time.Time{}
	}
	s := ordinalSuffix.ReplaceAllString(m[1], "$1")
	s = strings.ReplaceAll(s, "am", "AM")
	s = strings.ReplaceAll(s, "pm", "PM")
	for _, layout := range []string{"Jan 2, 2006 3:04 PM", "Jan 2, 2006 15:04", "January 2, 2006 3:04 PM"} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t
		}
	}
	return time.Time{}
}

// tailCap bounds what Tail keeps. The refusal is one line printed just
// before the process exits; if an engine ever pushes it out of the final
// 16KiB with trailing output, detection misses it and the failure degrades
// to a generic one.
const tailCap = 16 * 1024

// Tail is an io.Writer that keeps only the most recently written tailCap
// bytes, so classifying a failed run never means buffering all of its
// output alongside the on-disk log. It locks internally: an engine's stdout
// and stderr arrive from separate copy goroutines.
type Tail struct {
	mu  sync.Mutex
	buf []byte
}

func (t *Tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > tailCap {
		t.buf = t.buf[len(t.buf)-tailCap:]
	}
	return len(p), nil
}

func (t *Tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}

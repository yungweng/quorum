package usagelimit

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseResetAtHandlesOrdinalSuffix(t *testing.T) {
	line := "ERROR: You've hit your usage limit. Visit https://example.com/usage to purchase more credits or try again at Aug 10th, 2026 7:32 PM."
	got := ParseResetAt(line)
	want := time.Date(2026, time.August, 10, 19, 32, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("ParseResetAt = %v, want %v", got, want)
	}
}

func TestParseResetAtAcceptsDayWithoutOrdinalSuffix(t *testing.T) {
	got := ParseResetAt("try again at Aug 3, 2026 9:05 AM")
	want := time.Date(2026, time.August, 3, 9, 5, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("ParseResetAt = %v, want %v", got, want)
	}
}

func TestParseResetAtIsCaseInsensitiveToMeridiem(t *testing.T) {
	got := ParseResetAt("try again at Aug 1st, 2026 7:32 pm")
	want := time.Date(2026, time.August, 1, 19, 32, 0, 0, time.Local)
	if !got.Equal(want) {
		t.Fatalf("ParseResetAt = %v, want %v", got, want)
	}
}

func TestParseResetAtReturnsZeroTimeWhenTheClauseIsMissing(t *testing.T) {
	if got := ParseResetAt("ERROR: something else entirely"); !got.IsZero() {
		t.Fatalf("ParseResetAt = %v, want zero", got)
	}
	if got := ParseResetAt("try again at half past never"); !got.IsZero() {
		t.Fatalf("ParseResetAt on garbage clause = %v, want zero", got)
	}
}

func TestErrorUnwrapsToTheSentinel(t *testing.T) {
	var err error = &Error{ResetAt: time.Now()}
	if !errors.Is(err, Err) {
		t.Fatal("errors.Is(err, Err) = false")
	}
	var ul *Error
	if !errors.As(err, &ul) {
		t.Fatal("errors.As failed")
	}
}

func TestErrorMessageMentionsTheResetTime(t *testing.T) {
	err := &Error{ResetAt: time.Date(2026, time.August, 10, 19, 32, 0, 0, time.Local)}
	if !strings.Contains(err.Error(), "2026") {
		t.Fatalf("Error() = %q, want the reset time in it", err.Error())
	}
	if msg := (&Error{}).Error(); strings.Contains(msg, "resets") {
		t.Fatalf("zero ResetAt should not print a reset time, got %q", msg)
	}
}

// The refusal ends the output; the same phrase quoted mid-transcript, from
// reviewing code or docs that mention usage limits, is followed by more
// output and must never classify.
func TestRefusalLineMatchesOnlyTheClosingLines(t *testing.T) {
	closing := "transcript line\n\nERROR: You've hit your usage limit. Try again at Aug 10th, 2026 7:32 PM.\n\n"
	line, ok := RefusalLine(closing, "hit your usage limit")
	if !ok || !strings.HasPrefix(line, "ERROR:") {
		t.Fatalf("RefusalLine = (%q, %v), want the trimmed closing refusal", line, ok)
	}

	quoted := `the reviewed code contains "hit your usage limit" as a marker
transcript 1
transcript 2
transcript 3
transcript 4
transcript 5
ERROR: stream disconnected`
	if line, ok := RefusalLine(quoted, "hit your usage limit"); ok {
		t.Fatalf("a quoted mention mid-transcript classified as a refusal: %q", line)
	}
}

func TestTailWriterBoundsMemory(t *testing.T) {
	var tail Tail
	chunk := strings.Repeat("x", 1024)
	for range 64 {
		if _, err := tail.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	marker := "hit your usage limit right at the end"
	if _, err := tail.Write([]byte(marker)); err != nil {
		t.Fatal(err)
	}
	s := tail.String()
	if len(s) > tailCap {
		t.Fatalf("tail holds %d bytes, cap is %d", len(s), tailCap)
	}
	if !strings.Contains(s, marker) {
		t.Fatal("tail lost the most recent write")
	}
}

package runner

import (
	"bytes"
	"testing"
	"time"
)

// A log has no status line to update, so the review that posts beside a CI
// wait gets its own line; the ticks between two states stay out of it.
func TestLoopLogReporterLogsTheReviewBesideACIWaitOnChange(t *testing.T) {
	var out bytes.Buffer
	rep := &loopLogReporter{w: &out}

	rep.CIWait(42, 0, "review round 1 running")
	rep.CIWait(42, time.Second, "review round 1 running")
	rep.CIWait(42, 2*time.Second, "review round 1 posted")
	rep.CIWait(42, 3*time.Second, "review round 1 posted")

	want := "waiting for CI on PR #42... (review round 1 running)\n" +
		"waiting for CI on PR #42... (review round 1 posted)\n"
	if out.String() != want {
		t.Fatalf("log = %q, want %q", out.String(), want)
	}
}

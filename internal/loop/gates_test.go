package loop

import (
	"errors"
	"testing"
)

func TestDisputeGateRejectsEmptyRebuttal(t *testing.T) {
	for _, msg := range []string{
		MarkerDisputed,
		MarkerDisputed + "\n\n  ",
	} {
		r := &run{rep: NopReporter{}, lastMsg: msg}
		err := r.disputeGate("fix-round-1", "abc123")
		if !errors.Is(err, ErrNoProgress) {
			t.Errorf("disputeGate(%q) error = %v, want ErrNoProgress", msg, err)
		}
		if r.disputeAccepted {
			t.Errorf("disputeGate(%q) accepted an empty rebuttal", msg)
		}
	}
}

// Step labels name the step in the words the summary uses; a raw session tag
// on the timeline next to "Push fix 1" in the commit list read as two runs.
func TestStepLabelNamesEveryTagShape(t *testing.T) {
	for tag, want := range map[string]string{
		"fix-round-2":           "Fix round 2",
		"fix-round-2-dispute-1": "Dispute re-check 1",
		"fix-round-2-answers-3": "Answers 3",
		"ci-fix-1":              "CI fix 1",
		"push-fix-1":            "Push fix 1",
		"test-fix-2":            "Test fix 2",
		"suggestion-round":      "Suggestion round",
	} {
		if got := stepLabel(tag); got != want {
			t.Errorf("stepLabel(%q) = %q, want %q", tag, got, want)
		}
	}
}

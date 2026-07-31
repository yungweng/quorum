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

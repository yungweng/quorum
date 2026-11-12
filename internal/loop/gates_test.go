package loop

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/review"
)

// scriptedFixer plays one fix-session step per call: steps[i] runs for the
// i-th Exec or Resume and returns the final message the pipeline reads back.
type scriptedFixer struct {
	steps []func() string
	calls int
}

func (f *scriptedFixer) step(outFile string) error {
	if f.calls >= len(f.steps) {
		return errors.New("unexpected fix step")
	}
	msg := f.steps[f.calls]()
	f.calls++
	return os.WriteFile(outFile, []byte(msg), 0o644)
}

func (f *scriptedFixer) Exec(_ context.Context, _ envexec.Env, _ time.Duration, _, outFile string, _ io.Writer) (string, error) {
	return "fake-session", f.step(outFile)
}

func (f *scriptedFixer) Resume(_ context.Context, _ envexec.Env, _ time.Duration, _ string, _, outFile string, _ io.Writer) error {
	return f.step(outFile)
}

// A dispute accepted in one round says nothing about a later round. Here the
// first round's dispute is accepted, a base update sends the run into a second
// round, and that round's forced re-check turns up a real issue and commits a
// fix. The commit has never been reviewed, so the run must not finish on the
// first round's rebuttal.
func TestAcceptedDisputeDoesNotCarryIntoALaterRound(t *testing.T) {
	f := newBaseUpdateRepo(t)
	const dispute = "DISPUTED FINDINGS:\n1. The finding is wrong."
	fixer := &scriptedFixer{steps: []func() string{
		// Round 1: dispute; the base moves meanwhile.
		func() string { f.advanceBase(); return dispute },
		// Round 1 re-check: the dispute stands and is accepted.
		func() string { return dispute },
		// Round 2: dispute again.
		func() string { return dispute },
		// Round 2 re-check: the finding was real after all.
		func() string {
			if err := os.WriteFile(filepath.Join(f.worktree, "fix.txt"), []byte("fix\n"), 0o644); err != nil {
				t.Error(err)
			}
			f.git(f.worktree, "add", "fix.txt")
			f.git(f.worktree, "commit", "-q", "-m", "Fix the finding")
			return "Fixed the finding."
		},
	}}
	reviewer := reviewerFunc(func(_ context.Context, o review.Options) (*review.Result, error) {
		return &review.Result{
			Findings:    review.Findings{HeadSHA: o.HeadSHA, Blockers: 1},
			CommentBody: "## Blockers\n\n- A blocker.",
		}, nil
	})
	r := f.run(reviewer, 2)
	r.o.FixTimeout = time.Minute
	r.fixer = fixer
	r.msgDir = t.TempDir()

	res, err := r.execute()
	if res.DisputeAccepted || res.Converged {
		t.Fatalf("run finished on a stale dispute: converged %v, dispute accepted %v, text %q",
			res.Converged, res.DisputeAccepted, res.DisputeText)
	}
	if !errors.Is(err, ErrNotConverged) {
		t.Fatalf("execute error = %v, want ErrNotConverged", err)
	}
	if fixer.calls != len(fixer.steps) {
		t.Fatalf("fix steps = %d, want %d", fixer.calls, len(fixer.steps))
	}
}

func TestDisputeGateRejectsEmptyRebuttal(t *testing.T) {
	for _, msg := range []string{
		markerDisputed,
		markerDisputed + "\n\n  ",
	} {
		r := &run{rep: nopReporter{}}
		dispute, err := r.disputeGate(fixOutcome{tag: "fix-round-1", first: msg, last: msg}, "abc123")
		if !errors.Is(err, ErrNoProgress) {
			t.Errorf("disputeGate(%q) error = %v, want ErrNoProgress", msg, err)
		}
		if dispute.accepted {
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

// FIX_TIMEOUT=0 and --fix-timeout 0 are documented to switch the timeout off.
// Filling in the default for zero made that impossible.
func TestZeroFixTimeoutStaysOff(t *testing.T) {
	if got := (Options{}).withDefaults().FixTimeout; got != 0 {
		t.Fatalf("FixTimeout = %v, want 0 to mean no timeout", got)
	}
}

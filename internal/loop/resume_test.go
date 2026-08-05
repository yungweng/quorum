package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/usagelimit"
)

func TestResumableRunDirClassifiesRoundFailures(t *testing.T) {
	agg := &review.RunDirError{RunDir: "/run/1", Err: review.ErrAggregatorInvalid}
	if dir, ok := resumableRunDir(agg); !ok || dir != "/run/1" {
		t.Fatalf("aggregator failure = (%q, %v), want resumable", dir, ok)
	}
	ver := &review.RunDirError{RunDir: "/run/1", Err: review.ErrVerifierInvalid}
	if _, ok := resumableRunDir(ver); !ok {
		t.Fatal("verifier failure was not resumable")
	}
	limit := &review.RunDirError{RunDir: "/run/1", Err: &usagelimit.Error{}}
	if _, ok := resumableRunDir(limit); !ok {
		t.Fatal("usage-limit failure was not resumable")
	}
	if _, ok := resumableRunDir(review.ErrTooFewReviewers); ok {
		t.Fatal("a reviewer-phase failure was treated as resumable")
	}
	if _, ok := resumableRunDir(review.ErrAggregatorInvalid); ok {
		t.Fatal("a failure without a run dir was treated as resumable")
	}
	if _, ok := resumableRunDir(errors.New("boom")); ok {
		t.Fatal("a plain error was treated as resumable")
	}
}

// fakeReviewer feeds finishReview a scripted sequence of round outcomes and
// records the options of every call.
type fakeReviewer struct {
	calls   []review.Options
	results []struct {
		res *review.Result
		err error
	}
}

func (f *fakeReviewer) Run(_ context.Context, o review.Options) (*review.Result, error) {
	f.calls = append(f.calls, o)
	if len(f.results) == 0 {
		return nil, errors.New("fakeReviewer: no result queued")
	}
	next := f.results[0]
	f.results = f.results[1:]
	return next.res, next.err
}

func (f *fakeReviewer) queue(res *review.Result, err error) {
	f.results = append(f.results, struct {
		res *review.Result
		err error
	}{res, err})
}

func testRunWith(t *testing.T, fake *fakeReviewer) *run {
	t.Helper()
	return &run{
		p:   &Pipeline{Review: fake},
		o:   Options{},
		ctx: context.Background(),
		rep: NopReporter{},
	}
}

func commentFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "final-pr-comment.md")
	if err := os.WriteFile(path, []byte("## Summary\nfine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// An aggregator failure leaves six paid-for reviewer outputs on disk. The
// retry must reuse exactly that run directory instead of a fresh fan-out.
func TestFinishReviewRetriesOnceWithResumeRunAfterAnAggregatorFailure(t *testing.T) {
	fake := &fakeReviewer{}
	fake.queue(nil, &review.RunDirError{RunDir: "/run/1", Err: review.ErrAggregatorInvalid})
	fake.queue(&review.Result{RunDir: "/run/1", CommentFile: commentFile(t)}, nil)

	r := testRunWith(t, fake)
	_, comment, err := r.finishReview(1)
	if err != nil {
		t.Fatalf("finishReview: %v", err)
	}
	if comment == "" {
		t.Fatal("no comment came back from the resumed round")
	}
	if len(fake.calls) != 2 {
		t.Fatalf("review ran %d times, want 2", len(fake.calls))
	}
	if fake.calls[0].ResumeRun != "" {
		t.Fatal("the first attempt must not resume anything")
	}
	if fake.calls[1].ResumeRun != "/run/1" {
		t.Fatalf("retry resumed %q, want /run/1", fake.calls[1].ResumeRun)
	}
}

func TestFinishReviewDoesNotRetryATooFewReviewersFailure(t *testing.T) {
	fake := &fakeReviewer{}
	fake.queue(nil, review.ErrTooFewReviewers)

	r := testRunWith(t, fake)
	if _, _, err := r.finishReview(1); err == nil {
		t.Fatal("want the round failure")
	}
	if len(fake.calls) != 1 {
		t.Fatalf("review ran %d times, want 1", len(fake.calls))
	}
}

// A run handed a prior run's reviewer output (the agent's cross-poll resume
// after a usage limit) must actually reuse it for its first round.
func TestFirstRoundReusesAHandedInResumeDirectory(t *testing.T) {
	fake := &fakeReviewer{}
	fake.queue(&review.Result{RunDir: "/run/prior", CommentFile: commentFile(t)}, nil)

	r := testRunWith(t, fake)
	r.o.ResumeRun = "/run/prior"
	r.startReviewWith(1, r.o.ResumeRun)
	if _, _, err := r.finishReview(1); err != nil {
		t.Fatalf("finishReview: %v", err)
	}
	if len(fake.calls) != 1 || fake.calls[0].ResumeRun != "/run/prior" {
		t.Fatalf("review calls = %+v, want one resumed call", fake.calls)
	}
}

// A handed-in directory that turns out unusable says nothing about the pull
// request; the round must fall back to one fresh fan-out instead of failing.
func TestFirstRoundFallsBackToAFreshRunWhenTheResumeIsUnusable(t *testing.T) {
	for name, resumeErr := range map[string]error{
		"unusable": fmt.Errorf("%w: output directory does not exist", review.ErrResumeUnusable),
		"too few":  fmt.Errorf("%w: 2 available, need 3", review.ErrTooFewReviewers),
	} {
		t.Run(name, func(t *testing.T) {
			fake := &fakeReviewer{}
			fake.queue(nil, resumeErr)
			fake.queue(&review.Result{RunDir: "/run/2", CommentFile: commentFile(t)}, nil)

			r := testRunWith(t, fake)
			r.startReviewWith(1, "/run/stale")
			if _, _, err := r.finishReview(1); err != nil {
				t.Fatalf("finishReview: %v", err)
			}
			if len(fake.calls) != 2 {
				t.Fatalf("review ran %d times, want 2", len(fake.calls))
			}
			if fake.calls[0].ResumeRun != "/run/stale" || fake.calls[1].ResumeRun != "" {
				t.Fatalf("review calls = %+v, want a resumed call then a fresh one", fake.calls)
			}
		})
	}
}

func TestFinishReviewGivesUpAfterASecondFailure(t *testing.T) {
	fake := &fakeReviewer{}
	fake.queue(nil, &review.RunDirError{RunDir: "/run/1", Err: review.ErrAggregatorInvalid})
	fake.queue(nil, &review.RunDirError{RunDir: "/run/1", Err: review.ErrAggregatorInvalid})

	r := testRunWith(t, fake)
	if _, _, err := r.finishReview(1); !errors.Is(err, review.ErrAggregatorInvalid) {
		t.Fatalf("err = %v, want the aggregator failure", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("review ran %d times, want exactly 2", len(fake.calls))
	}
}

func TestFinishReviewRetriesOnceOnUsageLimit(t *testing.T) {
	fake := &fakeReviewer{}
	fake.queue(nil, &review.RunDirError{RunDir: "/run/1", Err: &usagelimit.Error{}})
	fake.queue(nil, &review.RunDirError{RunDir: "/run/1", Err: &usagelimit.Error{}})

	r := testRunWith(t, fake)
	_, _, err := r.finishReview(1)
	if !errors.Is(err, usagelimit.Err) {
		t.Fatalf("err = %v, want the usage-limit error to survive the wrapper", err)
	}
	if len(fake.calls) != 2 {
		t.Fatalf("review ran %d times, want 2", len(fake.calls))
	}
}

package review

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
)

func TestResumeFallbackEligibleClassification(t *testing.T) {
	for _, ok := range []error{ErrResumeUnusable, ErrTooFewReviewers} {
		if !ResumeFallbackEligible(ok) {
			t.Errorf("%v should allow the fresh-run fallback", ok)
		}
	}
	for _, no := range []error{ErrAggregatorInvalid, ErrVerifierInvalid, errors.New("boom")} {
		if ResumeFallbackEligible(no) {
			t.Errorf("%v must not trigger a fresh fan-out", no)
		}
	}
}

func TestResumableRunDirNeedsAnAggregationOrVerificationFailureWithItsRunDir(t *testing.T) {
	for _, err := range []error{ErrAggregatorInvalid, ErrVerifierInvalid} {
		if dir, ok := ResumableRunDir(&RunDirError{RunDir: "/run/1", Err: err}); !ok || dir != "/run/1" {
			t.Errorf("%v with its run directory = %q, %v", err, dir, ok)
		}
		if _, ok := ResumableRunDir(err); ok {
			t.Errorf("%v without a run directory counted as resumable", err)
		}
	}
	for _, err := range []error{ErrTooFewReviewers, ErrHeadDrifted, errors.New("boom")} {
		if _, ok := ResumableRunDir(&RunDirError{RunDir: "/run/1", Err: err}); ok {
			t.Errorf("%v counted as resumable", err)
		}
	}
}

func TestRunWithFallbackRunsOnceFreshAfterAnUnusableResume(t *testing.T) {
	o := Options{
		Repo: "acme/api", Number: 42, RepoRoot: t.TempDir(),
		RunsDir:   t.TempDir(),
		ResumeRun: filepath.Join(t.TempDir(), "gone"),
	}
	var fallbacks []error
	r := &Runner{GH: gh.New("/nonexistent/gh"), Git: git.New("/nonexistent/git")}
	_, err := r.RunWithFallback(t.Context(), o, func(err error) { fallbacks = append(fallbacks, err) })
	if len(fallbacks) != 1 || !errors.Is(fallbacks[0], ErrResumeUnusable) {
		t.Fatalf("fallback calls = %v, want one ErrResumeUnusable", fallbacks)
	}
	if err == nil || errors.Is(err, ErrResumeUnusable) {
		t.Fatalf("err = %v, want the fresh run's own failure", err)
	}

	fallbacks = nil
	o.ResumeRun = ""
	if _, err := r.RunWithFallback(t.Context(), o, func(err error) { fallbacks = append(fallbacks, err) }); err == nil {
		t.Fatal("a fresh run against missing tools succeeded")
	}
	if len(fallbacks) != 0 {
		t.Fatalf("a fresh run fell back: %v", fallbacks)
	}
}

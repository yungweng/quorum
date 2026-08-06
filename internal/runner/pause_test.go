package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/logbook"
	"github.com/yungweng/quorum/internal/paths"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/state"
)

func TestWritePauseThenReadPause(t *testing.T) {
	dir := t.TempDir()
	until := time.Now().Add(time.Hour).Truncate(time.Second)
	wrote, err := WritePause(dir, until)
	if err != nil {
		t.Fatalf("WritePause: %v", err)
	}
	if !wrote {
		t.Fatal("first pause did not report wrote")
	}
	got, ok := ReadPause(dir)
	if !ok || !got.Equal(until) {
		t.Fatalf("ReadPause = (%v, %v), want (%v, true)", got, ok, until)
	}
}

// Several reviews can hit the limit within moments of each other. Only the
// write that extends the deadline reports wrote, so exactly one notification
// goes out and a later reset time is never shortened.
func TestWritePauseKeepsTheLaterDeadline(t *testing.T) {
	dir := t.TempDir()
	later := time.Now().Add(2 * time.Hour).Truncate(time.Second)
	if _, err := WritePause(dir, later); err != nil {
		t.Fatal(err)
	}
	wrote, err := WritePause(dir, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if wrote {
		t.Fatal("an earlier deadline replaced a later one")
	}
	if got, _ := ReadPause(dir); !got.Equal(later) {
		t.Fatalf("pause = %v, want the later %v", got, later)
	}
}

func TestReadPauseIgnoresExpiredMissingAndGarbage(t *testing.T) {
	dir := t.TempDir()
	if _, ok := ReadPause(dir); ok {
		t.Fatal("a missing pause file read as paused")
	}
	if _, err := WritePause(dir, time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadPause(dir); ok {
		t.Fatal("an expired pause read as active")
	}
	if err := os.WriteFile(filepath.Join(dir, PauseFile), []byte("not a time\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := ReadPause(dir); ok {
		t.Fatal("garbage read as an active pause")
	}
}

func testRunner(t *testing.T) *Runner {
	t.Helper()
	dir := t.TempDir()
	return &Runner{
		P: paths.P{
			StateDir:    dir,
			StateFile:   filepath.Join(dir, "state.json"),
			HistoryFile: filepath.Join(dir, "history.jsonl"),
		},
		Log: logbook.New(filepath.Join(dir, "quorum.log")),
	}
}

// The refusal says nothing about the pull request, so it must neither count
// toward MaxRetries nor mark the request handled.
func TestRecordUsageLimitLeavesFailsAndReqAtUntouched(t *testing.T) {
	r := testRunner(t)
	key := "acme/api#42"
	if err := state.Mutate(r.P.StateFile, key, func(rec *state.Record) {
		rec.Fails = 2
	}); err != nil {
		t.Fatal(err)
	}

	until := time.Now().Add(time.Hour)
	r.recordUsageLimit(key, "abc123", "/run/1", "/log/1", until,
		&review.RunDirError{RunDir: "/run/1", Err: errors.New("usage limit reached")})

	st, err := state.Read(r.P.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	rec := st.PRs[key]
	if rec.Fails != 2 {
		t.Errorf("Fails = %d, want 2 (untouched)", rec.Fails)
	}
	if rec.ReqAt != "" {
		t.Errorf("ReqAt = %q, want empty (request not handled)", rec.ReqAt)
	}
	if rec.Status != state.Deferred {
		t.Errorf("Status = %q, want %q", rec.Status, state.Deferred)
	}
	if !rec.ResumableRun {
		t.Error("a run dir carried by the failure was not marked resumable")
	}
}

func TestResumableRunDirRequiresMatchingHeadAndOutput(t *testing.T) {
	r := testRunner(t)
	key := "acme/api#42"
	runDir := t.TempDir()
	seed := func(fn func(rec *state.Record)) {
		if err := state.Mutate(r.P.StateFile, key, fn); err != nil {
			t.Fatal(err)
		}
	}
	seed(func(rec *state.Record) {
		rec.ResumableRun = true
		rec.RunDir = runDir
		rec.SHA = "abc123"
	})

	if got := r.resumableRunDir(key, "abc123"); got != "" {
		t.Fatalf("a run dir without reviewer output resumed: %q", got)
	}
	if err := os.MkdirAll(filepath.Join(runDir, "output"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "output", "reviewer-1.md"), []byte("finding"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := r.resumableRunDir(key, "abc123"); got != runDir {
		t.Fatalf("resumableRunDir = %q, want %q", got, runDir)
	}
	if got := r.resumableRunDir(key, "def456"); got != "" {
		t.Fatalf("a different head resumed stale reviewer output: %q", got)
	}
	seed(func(rec *state.Record) { rec.ResumableRun = false })
	if got := r.resumableRunDir(key, "abc123"); got != "" {
		t.Fatalf("an unmarked record resumed: %q", got)
	}
}

// A babysit failure reports the pipeline's own root, which holds no reviewer
// output; the record must carry the review round's directory from the error
// chain instead, or the next poll's resume glob comes up empty and a full
// fresh fan-out is paid for again.
func TestRecordableRunDirPrefersTheReviewRunFromTheErrorChain(t *testing.T) {
	aggErr := fmt.Errorf("review round 1 failed: %w",
		&review.RunDirError{RunDir: "/review/run-1", Err: review.ErrAggregatorInvalid})
	if got := recordableRunDir("/babysit/root", aggErr); got != "/review/run-1" {
		t.Fatalf("recordableRunDir = %q, want the review run directory", got)
	}
	if got := recordableRunDir("/babysit/root", errors.New("fix session died")); got != "/babysit/root" {
		t.Fatalf("recordableRunDir = %q, want the reported directory", got)
	}
}

func TestResumeFallbackEligibleClassification(t *testing.T) {
	for _, ok := range []error{review.ErrResumeUnusable, review.ErrTooFewReviewers} {
		if !resumeFallbackEligible(ok) {
			t.Errorf("%v should allow the fresh-run fallback", ok)
		}
	}
	for _, no := range []error{review.ErrAggregatorInvalid, review.ErrVerifierInvalid, errors.New("boom")} {
		if resumeFallbackEligible(no) {
			t.Errorf("%v must not trigger a fresh fan-out", no)
		}
	}
}

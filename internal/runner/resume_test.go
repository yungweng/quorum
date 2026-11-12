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

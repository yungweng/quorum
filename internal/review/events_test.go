package review

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/engine"
	"github.com/yungweng/quorum/internal/envexec"
)

func TestReadProgress(t *testing.T) {
	runDir := t.TempDir()
	if _, ok := ReadProgress(runDir, 6); ok {
		t.Error("progress reported for a run with no event log")
	}
	if _, ok := ReadProgress("", 6); ok {
		t.Error("progress reported for an empty run directory")
	}

	out := filepath.Join(runDir, "output")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	// The event lines a run appends: "ok idx elapsed rank" and
	// "fail idx exit elapsed".
	events := "ok 2 614 1\nok 6 634 2\nfail 4 124 700\nok 3 711 3\n"
	if err := os.WriteFile(filepath.Join(out, "events.log"), []byte(events), 0o644); err != nil {
		t.Fatal(err)
	}
	p, ok := ReadProgress(runDir, 6)
	if !ok {
		t.Fatal("no progress read")
	}
	if p.Done != 3 || p.Failed != 1 || p.Requested != 6 {
		t.Errorf("progress = %+v", p)
	}
}

// A run that has launched its reviewers reports zero of them done, which is
// what tells a watcher the run is past preparing its worktree.
func TestReadProgressStarted(t *testing.T) {
	runDir := t.TempDir()
	out := filepath.Join(runDir, "output")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(out, "events.log"), []byte("start\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p, ok := ReadProgress(runDir, 6)
	if !ok {
		t.Fatal("a started run reported no progress")
	}
	if p.Done != 0 || p.Failed != 0 || p.Requested != 6 {
		t.Errorf("progress = %+v", p)
	}
}

// reviewByIndex stands in for a review engine: the reviewer whose output file
// is named in fail fails, every other one writes its output.
type reviewByIndex struct {
	engine.Reviewer
	fail string
}

func (e reviewByIndex) Review(_ context.Context, _ envexec.Env, _ time.Duration, _, _, outFile string, _ io.Writer) error {
	if filepath.Base(outFile) == e.fail {
		return errors.New("boom")
	}
	return os.WriteFile(outFile, []byte("finding"), 0o644)
}

// The dashboard reads what the run writes, from another process. The reader
// must count exactly what the reviewer fan-out and the later phases append.
func TestReadProgressReadsWhatTheRunWrites(t *testing.T) {
	run := testRunPaths(t)
	o := Options{Runs: 3, Concurrency: 1, ReviewTimeout: time.Minute}
	r := &Runner{}
	eng := reviewByIndex{fail: "reviewer-2.md"}
	if err := r.runReviewers(context.Background(), o, run, envexec.Env{Worktree: t.TempDir()}, eng, "origin/main", NopReporter{}); err != nil {
		t.Fatal(err)
	}
	p, ok := ReadProgress(run.root, 3)
	if !ok || p.Done != 2 || p.Failed != 1 || p.Requested != 3 || p.Phase != "" {
		t.Fatalf("after the reviewers: progress = %+v, ok = %v", p, ok)
	}
	for _, phase := range []string{PhaseAggregate, PhaseVerify} {
		appendEvent(run.output, phase)
		if p, _ := ReadProgress(run.root, 3); p.Phase != phase || p.Done != 2 || p.Failed != 1 {
			t.Fatalf("after %s: progress = %+v", phase, p)
		}
	}
}

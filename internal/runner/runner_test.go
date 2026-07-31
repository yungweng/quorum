package runner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yungweng/quorum/internal/paths"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/state"
)

// The scanner must find the run directory in a stream that arrives in
// arbitrary chunks, which is how a pipe delivers it.

func TestSafeName(t *testing.T) {
	if got := SafeName("crumbtray/toaster-api#2017"); got != "crumbtray-toaster-api-2017" {
		t.Errorf("got %q", got)
	}
}

func TestMarkerIsExclusive(t *testing.T) {
	dir := t.TempDir()
	key := "acme/api#1"

	m, got, err := Acquire(dir, key)
	if err != nil || !got {
		t.Fatalf("first Acquire: got=%v err=%v", got, err)
	}
	// This process is alive, so a second claim must fail.
	if _, got, _ := Acquire(dir, key); got {
		t.Error("the same review was claimed twice")
	}
	if live := Live(dir); len(live) != 1 || live[0].Key != key {
		t.Errorf("Live = %+v", live)
	}
	m.Release()
	if live := Live(dir); len(live) != 0 {
		t.Errorf("marker survived Release: %+v", live)
	}
}

func TestMarkerStaleIsReclaimed(t *testing.T) {
	dir := t.TempDir()
	key := "acme/api#2"
	// pid 0 is never a live process here, so this stands in for a crashed run.
	path := filepath.Join(dir, SafeName(key))
	if err := os.WriteFile(path, []byte("0\n"+key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if live := Live(dir); len(live) != 0 {
		t.Error("a dead marker counted as a running review")
	}
	if err := os.WriteFile(path, []byte("0\n"+key+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, got, err := Acquire(dir, key); !got || err != nil {
		t.Errorf("a stale marker blocked a new review: got=%v err=%v", got, err)
	}
}

func TestMarkerAdopt(t *testing.T) {
	dir := t.TempDir()
	m, _, err := Acquire(dir, "acme/api#3")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Adopt(os.Getpid()); err != nil {
		t.Fatal(err)
	}
	live := Live(dir)
	if len(live) != 1 || live[0].PID != os.Getpid() || live[0].Key != "acme/api#3" {
		t.Errorf("Live = %+v", live)
	}
}

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
	// The event lines pr-codex-review appends: "ok idx elapsed rank" and
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

func TestSummaryLine(t *testing.T) {
	if got := summaryLine(review.Findings{}); got != "nothing found" {
		t.Errorf("empty findings rendered as %q", got)
	}
	if got := summaryLine(review.Findings{Critical: 4}); got != "0 blockers, 4 critical, 0 suggestions" {
		t.Errorf("got %q", got)
	}
	// A run that only raised questions still has something to report.
	if got := summaryLine(review.Findings{Questions: 2}); got == "nothing found" {
		t.Error("questions were reported as nothing found")
	}
}

func TestAutoMergeFailureMarksRequestHandled(t *testing.T) {
	dir := t.TempDir()
	r := &Runner{P: paths.P{
		StateFile:   filepath.Join(dir, "state.json"),
		HistoryFile: filepath.Join(dir, "history.jsonl"),
	}}
	url := "https://example.invalid/comment/42"
	r.recordAutoMergeFailure("acme/api#42", "2026-07-31T09:00:00Z", "/run/42", review.Findings{
		PR: 42, HeadSHA: "abc123", Posted: true, CommentURL: &url, Suggestions: 2, Questions: 1,
	}, "auto-merge failed after the approval was posted")

	file, err := state.Read(r.P.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	rec := file.PRs["acme/api#42"]
	if rec.Status != state.Failed || rec.ReqAt != "2026-07-31T09:00:00Z" || rec.SHA != "abc123" {
		t.Fatalf("record = %+v", rec)
	}
	if rec.CommentURL != url || rec.Suggestions != 2 || rec.Questions != 1 || rec.Fails != 1 {
		t.Fatalf("review result was not preserved: %+v", rec)
	}
}

func TestLoadAvgIsPlausible(t *testing.T) {
	load, ok := LoadAvg1()
	if !ok {
		t.Skip("no load average on this platform")
	}
	if load < 0 || load > 1000 {
		t.Errorf("implausible load average: %v", load)
	}
}

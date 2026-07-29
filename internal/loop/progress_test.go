package loop

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/proc"
)

// claimedRun builds a run directory that reads as live, the way a real one
// does: claimed first, published second.
func claimedRun(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := proc.Claim(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
}

// What a run writes has to be what a watcher reads. The two sides live in
// different processes and never share a type at runtime, so nothing but this
// catches a field that stops surviving the round trip.
func TestPublishedProgressIsReadBack(t *testing.T) {
	root := filepath.Join(t.TempDir(), "acme-api-pr-7-20260101-000000")
	claimedRun(t, root)

	started := time.Now().Add(-30 * time.Minute).Round(time.Second)
	r := &run{root: root, prog: Progress{
		PID: os.Getpid(), Repo: "acme/api", Number: 7, Title: "a fix",
		Branch: "feature", StartedAt: started, MaxIter: 12, MaxCIFixes: 3,
		Round: 2, CI: CIGreen, Reviewed: true, Blockers: 1, Critical: 2, Commits: 4,
	}}
	r.enter(PhaseFix)

	got, ok := ReadProgress(root)
	if !ok {
		t.Fatal("a published run could not be read back")
	}
	if got.Repo != "acme/api" || got.Number != 7 || got.Key() != "acme/api#7" {
		t.Errorf("pull request identity was lost: %+v", got)
	}
	if got.PID != os.Getpid() {
		t.Errorf("pid = %d, want %d; without it a run the agent started cannot be matched to its record", got.PID, os.Getpid())
	}
	if got.Phase != PhaseFix || got.Since.IsZero() {
		t.Errorf("phase = %q since %v, want %q with a timestamp", got.Phase, got.Since, PhaseFix)
	}
	if !got.StartedAt.Equal(started) {
		t.Errorf("started at %v, want %v", got.StartedAt, started)
	}
	if got.Round != 2 || got.MaxIter != 12 || got.CI != CIGreen {
		t.Errorf("loop position was lost: %+v", got)
	}
	if !got.Reviewed || got.Blockers != 1 || got.Critical != 2 || got.Commits != 4 {
		t.Errorf("round result was lost: %+v", got)
	}
	if got.RunDir != root {
		t.Errorf("run dir = %q, want %q", got.RunDir, root)
	}
	if got.LogDir() != filepath.Join(root, "logs") {
		t.Errorf("log dir = %q", got.LogDir())
	}
}

// The progress file outlives the run: it stays in the run cache until
// collection drops the directory a week later. Only the claim says whether
// anything is still happening, so a finished run has to disappear on its own.
func TestLiveRunsIgnoresAFinishedRun(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "acme-api-pr-7-20260101-000000")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := proc.Claim(root)
	if err != nil {
		t.Fatal(err)
	}
	r := &run{root: root, prog: Progress{Repo: "acme/api", Number: 7}}
	r.enter(PhaseFix)

	if len(LiveRuns(dir)) != 1 {
		t.Fatal("a claimed run was not reported as live")
	}
	release()
	if got := LiveRuns(dir); len(got) != 0 {
		t.Errorf("a finished run is still on the dashboard: %+v", got)
	}
}

// A run claims its directory before it publishes. Reporting it in between puts
// a line with no repository and no number on screen.
func TestLiveRunsIgnoresARunThatHasNotPublished(t *testing.T) {
	dir := t.TempDir()
	claimedRun(t, filepath.Join(dir, "acme-api-pr-7-20260101-000000"))

	if got := LiveRuns(dir); len(got) != 0 {
		t.Errorf("a run without a progress file was reported: %+v", got)
	}
}

// Oldest first, so a run does not jump around the screen as others come and go.
func TestLiveRunsAreOrderedByStart(t *testing.T) {
	dir := t.TempDir()
	base := time.Now().Add(-time.Hour).Round(time.Second)
	for i, name := range []string{"c", "a", "b"} {
		root := filepath.Join(dir, name)
		claimedRun(t, root)
		r := &run{root: root, prog: Progress{
			Repo: "acme/api", Number: i, StartedAt: base.Add(time.Duration(i) * time.Minute),
		}}
		r.publish()
	}
	got := LiveRuns(dir)
	if len(got) != 3 {
		t.Fatalf("got %d runs, want 3", len(got))
	}
	for i, p := range got {
		if p.Number != i {
			t.Errorf("run %d is #%d, want #%d: %v", i, p.Number, i, got)
		}
	}
}

// Publishing must never take a run down with it. A read-only cache directory
// is a reason to lose the dashboard line, not the fix loop.
func TestPublishSurvivesAnUnwritableRunDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "gone")
	r := &run{root: root, prog: Progress{Repo: "acme/api", Number: 7}}
	r.enter(PhaseFix) // the directory does not exist
	if _, ok := ReadProgress(root); ok {
		t.Error("progress was published into a directory that is not there")
	}
}

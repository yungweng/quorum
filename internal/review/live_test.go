package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/engine/spec"
	"github.com/yungweng/quorum/internal/proc"
)

type warningReporter struct {
	NopReporter
	warnings []string
}

func (r *warningReporter) Warn(s string) { r.warnings = append(r.warnings, s) }

func TestTrackLivePublishesUntilClosed(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := proc.Claim(runDir)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	liveDir := filepath.Join(root, "live")

	rep := TrackLive(NopReporter{}, liveDir)
	rep.Header(RunHeader{
		Repo: "acme/api", Number: 42, Title: "tenant scoping", Author: "example-user",
		Runs: 3, RunDir: runDir,
		Model: spec.Model{Engine: spec.Codex, Name: "gpt-5.6-terra", Effort: "max"},
	})

	runs := LiveRuns(liveDir)
	if len(runs) != 1 {
		t.Fatalf("LiveRuns returned %d runs, want 1", len(runs))
	}
	got := runs[0]
	if got.Key() != "acme/api#42" || got.Title != "tenant scoping" ||
		got.Author != "example-user" || got.Reviewers != 3 {
		t.Errorf("published run = %+v", got)
	}
	if got.PID != os.Getpid() || got.RunDir != runDir || got.StartedAt.IsZero() {
		t.Errorf("published liveness metadata = %+v", got)
	}
	// The dashboard has no other way to name what this run is spending: reading
	// the current configuration would answer for whatever was set last, not for
	// what this run started with.
	if got.Model.Tag() != "gpt-5.6-terra/max" {
		t.Errorf("published model = %q, want gpt-5.6-terra/max", got.Model.Tag())
	}

	rep.Close()
	if runs := LiveRuns(liveDir); len(runs) != 0 {
		t.Errorf("finished run remained live: %+v", runs)
	}
}

// claimedRunDir returns a run directory whose claim this process holds, so a
// record pointing at it fails only on its own fields or its PID.
func claimedRunDir(t *testing.T) string {
	t.Helper()
	runDir := filepath.Join(t.TempDir(), "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	release, err := proc.Claim(runDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return runDir
}

func TestLiveRunsSkipsIncompleteMetadata(t *testing.T) {
	dir := t.TempDir()
	// Live process, claimed run directory, but no reviewer count.
	if err := publishLive(dir, filepath.Join(dir, "run.json"), LiveRun{
		PID: os.Getpid(), Repo: "acme/api", Number: 42,
		StartedAt: time.Now(), RunDir: claimedRunDir(t),
	}); err != nil {
		t.Fatal(err)
	}

	if runs := LiveRuns(dir); len(runs) != 0 {
		t.Errorf("LiveRuns accepted incomplete metadata: %+v", runs)
	}
}

func TestLiveRunsAcceptsABranchWithoutAPRNumber(t *testing.T) {
	liveDir := t.TempDir()
	rep := TrackLive(NopReporter{}, liveDir)
	rep.Header(RunHeader{
		Repo: "acme/api", Branch: "feature/crumb-tray", Title: "feature/crumb-tray",
		Runs: 3, RunDir: claimedRunDir(t),
	})
	defer rep.Close()

	runs := LiveRuns(liveDir)
	if len(runs) != 1 {
		t.Fatalf("branch-only live review was rejected: %+v", runs)
	}
	if got := runs[0].Key(); got != "acme/api@feature/crumb-tray" {
		t.Errorf("branch key = %q", got)
	}
}

func TestLiveRunsRemovesADeadProcessRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "999999.json")
	// The run directory is still claimed, so only the dead PID can retire it.
	if err := publishLive(dir, path, LiveRun{
		PID: 999999, Repo: "acme/api", Number: 42, Title: "tenant scoping",
		StartedAt: time.Now(), Reviewers: 3, RunDir: claimedRunDir(t),
	}); err != nil {
		t.Fatal(err)
	}

	if runs := LiveRuns(dir); len(runs) != 0 {
		t.Errorf("dead process remained live: %+v", runs)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("dead process record was not removed: %v", err)
	}
}

func TestTrackLiveWarnsWhenItCannotPublish(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rep := &warningReporter{}
	tracker := TrackLive(rep, filepath.Join(blocker, "live"))

	tracker.Header(RunHeader{
		Repo: "acme/api", Number: 42, Title: "tenant scoping",
		Runs: 3, RunDir: filepath.Join(root, "run"),
	})

	if len(rep.warnings) != 1 || !strings.Contains(rep.warnings[0], "dashboard tracking unavailable") {
		t.Errorf("warnings = %q", rep.warnings)
	}
}

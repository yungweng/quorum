package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/engine"
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
		Engine: engine.Codex, Model: "gpt-5.6-terra", Effort: "max",
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

func TestReadLiveRejectsIncompleteMetadata(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.json")
	if err := publishLive(dir, path, LiveRun{
		PID: os.Getpid(), Repo: "acme/api", Number: 42,
		StartedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if got, ok := readLive(path); ok {
		t.Errorf("ReadLive accepted incomplete metadata: %+v", got)
	}
}

func TestReadLiveAcceptsABranchWithoutAPRNumber(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "run.json")
	if err := publishLive(dir, path, LiveRun{
		PID: os.Getpid(), Repo: "acme/api", Branch: "feature/crumb-tray",
		Title: "feature/crumb-tray", StartedAt: time.Now(), Reviewers: 3,
		RunDir: filepath.Join(dir, "run"),
	}); err != nil {
		t.Fatal(err)
	}

	got, ok := readLive(path)
	if !ok {
		t.Fatal("branch-only live review was rejected")
	}
	if got.Key() != "acme/api@feature/crumb-tray" {
		t.Errorf("branch key = %q", got.Key())
	}
}

func TestLiveRunsRemovesADeadProcessRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "999999.json")
	if err := publishLive(dir, path, LiveRun{
		PID: 999999, Repo: "acme/api", Number: 42, Title: "tenant scoping",
		StartedAt: time.Now(), Reviewers: 3, RunDir: "/tmp/run",
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

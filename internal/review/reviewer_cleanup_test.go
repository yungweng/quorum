package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReviewerCleanupRemovesUntrackedArtifacts(t *testing.T) {
	run, _, _, g, head := fakeVerifier(t, goodComment, false)
	artifact := filepath.Join(run.worktree, ".pnpm-store", "v11", "index.db")
	if err := os.MkdirAll(filepath.Dir(artifact), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifact, []byte("cache\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rep := &warningReporter{}

	if err := (&Runner{Git: g}).cleanReviewerArtifacts(context.Background(), run, head, nil, rep); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(run.worktree, ".pnpm-store")); !os.IsNotExist(err) {
		t.Fatalf("reviewer artifact remains: %v", err)
	}
	if status, err := g.StatusPorcelain(context.Background(), run.worktree); err != nil || status != "" {
		t.Fatalf("status after cleanup = %q, %v", status, err)
	}
	logPath := filepath.Join(run.output, "reviewer-cleanup.log")
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "?? .pnpm-store/") {
		t.Fatalf("cleanup log does not identify the artifact: %q", log)
	}
	if len(rep.warnings) != 1 || !strings.Contains(rep.warnings[0], logPath) {
		t.Fatalf("cleanup warnings = %q", rep.warnings)
	}
}

func TestReviewerCleanupRejectsTrackedChangesWithoutResettingThem(t *testing.T) {
	for _, staged := range []bool{false, true} {
		name := "unstaged"
		if staged {
			name = "staged"
		}
		t.Run(name, func(t *testing.T) {
			run, _, _, g, head := fakeVerifier(t, goodComment, false)
			seed := filepath.Join(run.worktree, "seed.txt")
			if err := os.WriteFile(seed, []byte("changed\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if staged {
				runTestGit(t, run.worktree, "add", "seed.txt")
			}

			err := (&Runner{Git: g}).cleanReviewerArtifacts(context.Background(), run, head, nil, NopReporter{})
			if err == nil || !strings.Contains(err.Error(), "tracked changes") {
				t.Fatalf("cleanup error = %v, want tracked-change rejection", err)
			}
			content, readErr := os.ReadFile(seed)
			if readErr != nil || string(content) != "changed\n" {
				t.Fatalf("tracked change was reset: content %q, error %v", content, readErr)
			}
		})
	}
}

func TestReviewerCleanupToleratesRecordedSetupDrift(t *testing.T) {
	run, _, _, g, head := fakeVerifier(t, goodComment, false)
	seed := filepath.Join(run.worktree, "seed.txt")
	if err := os.WriteFile(seed, []byte("rewritten by setup\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setup, err := (&Runner{Git: g}).captureSetupDrift(context.Background(), run.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if got := setup.paths(); len(got) != 1 || got[0] != "seed.txt" {
		t.Fatalf("captured drift paths = %v", got)
	}

	if err := (&Runner{Git: g}).cleanReviewerArtifacts(context.Background(), run, head, setup, NopReporter{}); err != nil {
		t.Fatalf("cleanup with recorded setup drift: %v", err)
	}
	if err := (&Runner{Git: g}).worktreeUnchanged(context.Background(), run.worktree, head, setup); err != nil {
		t.Fatalf("worktreeUnchanged with recorded setup drift: %v", err)
	}
	content, err := os.ReadFile(seed)
	if err != nil || string(content) != "rewritten by setup\n" {
		t.Fatalf("setup drift was reset: content %q, error %v", content, err)
	}
}

func TestReviewerCleanupRejectsDifferentContentOnASetupDriftPath(t *testing.T) {
	run, _, _, g, head := fakeVerifier(t, goodComment, false)
	seed := filepath.Join(run.worktree, "seed.txt")
	if err := os.WriteFile(seed, []byte("rewritten by setup\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setup, err := (&Runner{Git: g}).captureSetupDrift(context.Background(), run.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(seed, []byte("edited by a reviewer\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = (&Runner{Git: g}).cleanReviewerArtifacts(context.Background(), run, head, setup, NopReporter{})
	if err == nil || !strings.Contains(err.Error(), "tracked changes") {
		t.Fatalf("cleanup error = %v, want tracked-change rejection", err)
	}
}

func TestReviewerCleanupRejectsOtherFilesDespiteSetupDrift(t *testing.T) {
	run, _, _, g, _ := fakeVerifier(t, goodComment, false)
	other := filepath.Join(run.worktree, "other.txt")
	if err := os.WriteFile(other, []byte("second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, run.worktree, "add", "other.txt")
	runTestGit(t, run.worktree, "commit", "-q", "-m", "second tracked file")
	head := runTestGit(t, run.worktree, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(run.worktree, "seed.txt"), []byte("rewritten by setup\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	setup, err := (&Runner{Git: g}).captureSetupDrift(context.Background(), run.worktree)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(other, []byte("edited by a reviewer\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err = (&Runner{Git: g}).cleanReviewerArtifacts(context.Background(), run, head, setup, NopReporter{})
	if err == nil || !strings.Contains(err.Error(), "other.txt") {
		t.Fatalf("cleanup error = %v, want rejection naming other.txt", err)
	}
}

func TestCaptureSetupDriftRejectsARename(t *testing.T) {
	run, _, _, g, _ := fakeVerifier(t, goodComment, false)
	runTestGit(t, run.worktree, "mv", "seed.txt", "moved.txt")

	_, err := (&Runner{Git: g}).captureSetupDrift(context.Background(), run.worktree)
	if err == nil || !strings.Contains(err.Error(), "cannot tolerate") {
		t.Fatalf("capture error = %v, want intolerable-change rejection", err)
	}
}

func TestReviewerCleanupRejectsChangedHead(t *testing.T) {
	run, _, _, g, expectedHead := fakeVerifier(t, goodComment, false)
	if err := os.WriteFile(filepath.Join(run.worktree, "next.txt"), []byte("next\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, run.worktree, "add", "next.txt")
	runTestGit(t, run.worktree, "commit", "-q", "-m", "unexpected")

	err := (&Runner{Git: g}).cleanReviewerArtifacts(context.Background(), run, expectedHead, nil, NopReporter{})
	if err == nil || !strings.Contains(err.Error(), "changed HEAD") {
		t.Fatalf("cleanup error = %v, want changed-HEAD rejection", err)
	}
}

func TestReviewerCleanupFailsWhenGitCannotRemoveAnUntrackedRepository(t *testing.T) {
	run, _, _, g, head := fakeVerifier(t, goodComment, false)
	nested := filepath.Join(run.worktree, "generated-repository")
	if err := os.Mkdir(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	runTestGit(t, nested, "init", "-q")

	err := (&Runner{Git: g}).cleanReviewerArtifacts(context.Background(), run, head, nil, NopReporter{})
	if err == nil || !strings.Contains(err.Error(), "cleanup incomplete") {
		t.Fatalf("cleanup error = %v, want incomplete-cleanup rejection", err)
	}
}

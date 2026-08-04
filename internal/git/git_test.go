package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// conflictRepo builds a real repository whose main and feature branches edited
// the same line, plus a clean branch that did not.
func conflictRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "-q", "-b", "main")
	write("shared.txt", "base\n")
	run("add", "shared.txt")
	run("commit", "-q", "-m", "base")
	run("checkout", "-q", "-b", "feature")
	write("shared.txt", "feature\n")
	run("commit", "-q", "-am", "feature change")
	run("checkout", "-q", "-b", "clean", "main")
	write("other.txt", "clean\n")
	run("add", "other.txt")
	run("commit", "-q", "-m", "clean change")
	run("checkout", "-q", "main")
	write("shared.txt", "main\n")
	run("commit", "-q", "-am", "main change")
	return dir
}

func TestMergeConflictsProbe(t *testing.T) {
	dir := conflictRepo(t)
	g := New("git")

	conflicted, err := g.MergeConflicts(context.Background(), dir, "main", "feature")
	if err != nil {
		t.Fatal(err)
	}
	if !conflicted {
		t.Error("both sides edited the same line, but no conflict was reported")
	}

	conflicted, err = g.MergeConflicts(context.Background(), dir, "main", "clean")
	if err != nil {
		t.Fatal(err)
	}
	if conflicted {
		t.Error("independent changes were reported as a conflict")
	}
}

func TestMergeConflictsProbeReportsUnknownRefsAsErrors(t *testing.T) {
	dir := conflictRepo(t)
	g := New("git")
	if _, err := g.MergeConflicts(context.Background(), dir, "main", "no-such-branch"); err == nil {
		t.Fatal("an unresolvable ref must be an error, not an answer")
	}
}

func TestCleanUntrackedPreservesTrackedAndIgnoredFiles(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	run("init", "-q")
	write(".gitignore", "ignored/\n")
	write("tracked.txt", "tracked\n")
	run("add", ".gitignore", "tracked.txt")
	run("commit", "-q", "-m", "fixture")
	write("generated/cache.db", "generated\n")
	write("ignored/cache.db", "ignored\n")

	g := New("git")
	if err := g.CleanUntracked(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "generated")); !os.IsNotExist(err) {
		t.Fatalf("untracked directory remains: %v", err)
	}
	for _, path := range []string{"tracked.txt", "ignored/cache.db"} {
		if _, err := os.Stat(filepath.Join(dir, path)); err != nil {
			t.Errorf("%s was removed: %v", path, err)
		}
	}
}

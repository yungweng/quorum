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

func TestShowFileDistinguishesMissingPathsFromInvalidRevisions(t *testing.T) {
	dir := conflictRepo(t)
	g := New("git")

	content, ok, err := g.ShowFile(context.Background(), dir, "main", "missing.txt")
	if err != nil || ok || content != "" {
		t.Fatalf("ShowFile missing path = %q, %v, %v; want empty, false, nil", content, ok, err)
	}

	if _, _, err := g.ShowFile(context.Background(), dir, "not-a-revision", "shared.txt"); err == nil {
		t.Fatal("ShowFile accepted an invalid revision as a missing file")
	}
}

func TestShowFilePropagatesPathProbeFailures(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
case "$*" in
  "rev-parse -q --verify main^{tree}") echo tree ;;
  "ls-tree --name-only main -- blocked.txt") echo unavailable >&2; exit 1 ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if _, _, err := New(bin).ShowFile(context.Background(), t.TempDir(), "main", "blocked.txt"); err == nil {
		t.Fatal("ShowFile accepted a path-probe failure as a missing file")
	}
}

// The diverged-checkout stop leans on this distinction: a local branch that is
// merely behind origin may proceed, one with commits of its own may not.
func TestIsAncestorDistinguishesBehindFromDiverged(t *testing.T) {
	dir := conflictRepo(t)
	g := G{Bin: "git"}
	ctx := context.Background()

	if ok, err := g.IsAncestor(ctx, dir, "main~1", "main"); err != nil || !ok {
		t.Fatalf("IsAncestor(main~1, main) = (%v, %v), want true", ok, err)
	}
	if ok, err := g.IsAncestor(ctx, dir, "feature", "main"); err != nil || ok {
		t.Fatalf("IsAncestor(feature, main) = (%v, %v), want false for a diverged branch", ok, err)
	}
	if _, err := g.IsAncestor(ctx, dir, "not-a-rev", "main"); err == nil {
		t.Fatal("an unresolvable revision was not reported as an error")
	}
}

// The round summary and the fix-log comment list a round's commits with this.
// A round that merged the base branch must not claim the base's commits as its
// own work: only the first-parent line belongs to the round.
func TestLogOnelineFollowsFirstParent(t *testing.T) {
	dir := conflictRepo(t)
	g := G{Bin: "git"}
	ctx := context.Background()
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
	// The clean branch merges main, which brings in "main change".
	run("checkout", "-q", "clean")
	run("merge", "-q", "--no-edit", "main")

	out := g.LogOneline(ctx, dir, "main~1..clean")
	if !strings.Contains(out, "clean change") {
		t.Fatalf("the branch's own commit is missing:\n%s", out)
	}
	if strings.Contains(out, "main change") {
		t.Fatalf("a commit merged in from the base is listed as the branch's own:\n%s", out)
	}
}

func TestIsolateHooksSnapshotsAWorktreeAwayFromSharedHookChanges(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := conflictRepo(t)
	shared := filepath.Join(dir, ".git", "hooks", "pre-push")
	if err := os.WriteFile(shared, []byte("#!/bin/sh\necho first\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	worktree := filepath.Join(t.TempDir(), "worktree")
	cmd := exec.Command("git", "worktree", "add", "--quiet", "--detach", worktree, "HEAD")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}

	g := New("git")
	isolated, err := g.IsolateHooks(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	want := "#!/bin/sh\necho first\n"
	isolatedHook := filepath.Join(isolated, "pre-push")
	if data, err := os.ReadFile(isolatedHook); err != nil || string(data) != want {
		t.Fatalf("isolated hook = %q, %v; want %q", data, err, want)
	}
	if info, err := os.Stat(isolatedHook); err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("isolated hook mode = %v, %v; want 0755", info, err)
	}

	if err := os.WriteFile(shared, []byte("#!/bin/sh\necho second\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(isolatedHook); err != nil || string(data) != want {
		t.Fatalf("shared rewrite reached isolated hook: %q, %v", data, err)
	}
	resumed, err := g.IsolateHooks(context.Background(), worktree)
	if err != nil || resumed != isolated {
		t.Fatalf("resumed hook snapshot = %q, %v; want %q", resumed, err, isolated)
	}
	if data, err := os.ReadFile(isolatedHook); err != nil || string(data) != want {
		t.Fatalf("resume replaced the isolated hook: %q, %v", data, err)
	}
	resolved, err := g.WithHooksPath(isolated).PrePushPath(context.Background(), worktree)
	if err != nil || resolved != isolatedHook {
		t.Fatalf("scoped pre-push path = %q, %v; want %q", resolved, err, isolatedHook)
	}
}

func TestIsolateHooksCopiesConfiguredRelativePathsAndInternalSymlinks(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := conflictRepo(t)
	worktree := filepath.Join(t.TempDir(), "worktree")
	cmd := exec.Command("git", "worktree", "add", "--quiet", "--detach", worktree, "HEAD")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "config", "core.hooksPath", ".githooks")
	cmd.Dir = worktree
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config core.hooksPath: %v\n%s", err, out)
	}
	hooks := filepath.Join(worktree, ".githooks")
	if err := os.Mkdir(hooks, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "runner"), []byte("#!/bin/sh\necho stable\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("runner", filepath.Join(hooks, "pre-push")); err != nil {
		t.Fatal(err)
	}

	isolated, err := New("git").IsolateHooks(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	isolatedHook := filepath.Join(isolated, "pre-push")
	if data, err := os.ReadFile(isolatedHook); err != nil || string(data) != "#!/bin/sh\necho stable\n" {
		t.Fatalf("isolated symlink = %q, %v", data, err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "runner"), []byte("#!/bin/sh\necho changed\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(isolatedHook); err != nil || string(data) != "#!/bin/sh\necho stable\n" {
		t.Fatalf("configured hook rewrite reached isolated symlink: %q, %v", data, err)
	}
}

func TestIsolateHooksAcceptsAMissingConfiguredHookDirectory(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := conflictRepo(t)
	worktree := filepath.Join(t.TempDir(), "worktree")
	cmd := exec.Command("git", "worktree", "add", "--quiet", "--detach", worktree, "HEAD")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	cmd = exec.Command("git", "config", "core.hooksPath", ".missing-hooks")
	cmd.Dir = worktree
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git config core.hooksPath: %v\n%s", err, out)
	}

	isolated, err := New("git").IsolateHooks(context.Background(), worktree)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(isolated)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("isolated missing hook directory contains %d entries", len(entries))
	}
}

func TestPushPinsTheVerifiedSHAAndSkipsTheSecondHookRun(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "args")
	bin := filepath.Join(dir, "git")
	script := `#!/bin/sh
printf '%s\n' "$@" > "` + argsPath + `"
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	if out, err := New(bin).Push(context.Background(), dir, "origin", "feature/crumb-tray", "verified-sha"); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "push\n-q\n--no-verify\norigin\nverified-sha:refs/heads/feature/crumb-tray\n"
	if got := string(data); got != want {
		t.Fatalf("git arguments = %q, want %q", got, want)
	}
}

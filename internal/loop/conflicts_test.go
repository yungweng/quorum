package loop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/codex"
	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/target"
)

func writeTool(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// warnRecorder keeps what the run warned about and discards the rest.
type warnRecorder struct {
	NopReporter
	warns []string
}

func (w *warnRecorder) Warn(s string) { w.warns = append(w.warns, s) }

func TestRefuseDraft(t *testing.T) {
	draft := gh.FullPR{Number: 42, IsDraft: true}
	ready := gh.FullPR{Number: 42}

	if err := refuseDraft(draft, false, false); err == nil ||
		!strings.Contains(err.Error(), "--draft") {
		t.Fatalf("draft without the override = %v, want a refusal naming --draft", err)
	}
	if err := refuseDraft(draft, false, true); err != nil {
		t.Fatalf("draft with the override = %v", err)
	}
	if err := refuseDraft(ready, false, false); err != nil {
		t.Fatalf("ready PR = %v", err)
	}
	// A branch-only target has no PR whose draft state could mean anything.
	if err := refuseDraft(draft, true, false); err != nil {
		t.Fatalf("branch-only = %v", err)
	}
}

func TestLocalRunRejectsAPullRequestNumber(t *testing.T) {
	o := Options{Repo: "acme/api", RepoRoot: "/tmp/x", MaxIter: 1, Local: true, Number: 42}
	if err := o.validate(); err == nil ||
		!strings.Contains(err.Error(), "local") {
		t.Fatalf("validate = %v, want a refusal of the PR number", err)
	}
	o.Number = 0
	if err := o.validate(); err != nil {
		t.Fatalf("validate without a number = %v", err)
	}
}

func TestConflictCheckIsOffWhenDisabled(t *testing.T) {
	// Any git call fails loudly, so a disabled check must not make one.
	bin := writeTool(t, "git", `echo "unexpected git call: $*" >&2; exit 1`)
	r := &run{
		p:   &Pipeline{Git: git.New(bin)},
		o:   Options{ResolveConflicts: false},
		ctx: context.Background(),
		rep: NopReporter{},
	}
	if err := r.ensureMergeable(); err != nil {
		t.Fatal(err)
	}
}

func TestConflictCheckDegradesWhenGitCannotAnswer(t *testing.T) {
	// merge-tree --write-tree needs git 2.38. An older git must degrade the
	// run to "unchecked", not kill it.
	bin := writeTool(t, "git", `
case "$*" in
  "fetch -q origin +refs/heads/main:refs/remotes/origin/main") ;;
  "merge-tree --write-tree --name-only origin/main HEAD") echo "usage: git merge-tree" >&2; exit 129 ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac`)
	rep := &warnRecorder{}
	r := &run{
		p:        &Pipeline{Git: git.New(bin)},
		o:        Options{ResolveConflicts: true, RepoRoot: t.TempDir()},
		ctx:      context.Background(),
		rep:      rep,
		pr:       gh.FullPR{BaseRefName: "main"},
		worktree: t.TempDir(),
	}
	if err := r.ensureMergeable(); err != nil {
		t.Fatal(err)
	}
	if len(rep.warns) != 1 || !strings.Contains(rep.warns[0], "cannot check for merge conflicts") {
		t.Fatalf("warns = %q", rep.warns)
	}
}

func TestCleanBranchNeverStartsAConflictSession(t *testing.T) {
	bin := writeTool(t, "git", `
case "$*" in
  "fetch -q origin +refs/heads/main:refs/remotes/origin/main") ;;
  "merge-tree --write-tree --name-only origin/main HEAD") echo "treehash" ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac`)
	r := &run{
		p:        &Pipeline{Git: git.New(bin)},
		o:        Options{ResolveConflicts: true, RepoRoot: t.TempDir()},
		ctx:      context.Background(),
		rep:      NopReporter{},
		pr:       gh.FullPR{BaseRefName: "main"},
		worktree: t.TempDir(),
	}
	if err := r.ensureMergeable(); err != nil {
		t.Fatal(err)
	}
	if r.conflictFixes != 0 {
		t.Fatal("a clean branch started a conflict session")
	}
}

func TestConflictSessionThatCommitsNothingStopsTheRun(t *testing.T) {
	gitBin := writeTool(t, "git", `
case "$*" in
  "fetch -q origin +refs/heads/main:refs/remotes/origin/main") ;;
  "merge-tree --write-tree --name-only origin/main HEAD") echo "shared.txt" ; exit 1 ;;
  "rev-parse HEAD") echo "same-sha" ;;
  "status --porcelain") ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac`)
	codexBin := writeTool(t, "codex", `
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
printf 'nothing to do\n' > "$out"`)

	root := t.TempDir()
	r := &run{
		p:   &Pipeline{Git: git.New(gitBin)},
		o:   Options{ResolveConflicts: true, RepoRoot: t.TempDir(), FixTimeout: time.Minute},
		ctx: context.Background(),
		rep: NopReporter{},
		pr:  gh.FullPR{Number: 42, BaseRefName: "main"},
		// A preset session id keeps codexCall on the resume path, so the test
		// does not depend on Codex's session files.
		sessionID: "session-under-test",
		branch:    "feature",
		worktree:  t.TempDir(),
		logDir:    filepath.Join(root, "logs"),
		msgDir:    filepath.Join(root, "messages"),
		codex:     codex.Options{Bin: codexBin},
	}
	for _, d := range []string{r.logDir, r.msgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.env = envexec.Env{Worktree: r.worktree}

	err := r.ensureMergeable()
	if !errors.Is(err, ErrConflicts) {
		t.Fatalf("ensureMergeable = %v, want ErrConflicts", err)
	}
}

// TestConflictResolutionMergesAndPushes drives the full path against real
// repositories: a feature branch that conflicts with main, a fake Codex that
// performs the merge, and a bare origin the result must arrive on.
func TestConflictResolutionMergesAndPushes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	gitEnv := append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	gitIn := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = gitEnv
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	origin := t.TempDir()
	gitIn(origin, "init", "-q", "--bare", "-b", "main")

	clone := t.TempDir()
	gitIn(clone, "init", "-q", "-b", "main")
	gitIn(clone, "remote", "add", "origin", origin)
	if err := os.WriteFile(filepath.Join(clone, "shared.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(clone, "add", "shared.txt")
	gitIn(clone, "commit", "-q", "-m", "base")
	gitIn(clone, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(clone, "shared.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(clone, "commit", "-q", "-am", "feature change")
	gitIn(clone, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(clone, "shared.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(clone, "commit", "-q", "-am", "main change")
	gitIn(clone, "push", "-q", "origin", "main", "feature")
	gitIn(clone, "checkout", "-q", "feature")
	featureSHA := gitIn(clone, "rev-parse", "HEAD")

	worktree := filepath.Join(t.TempDir(), "worktree")
	gitIn(clone, "worktree", "add", "--quiet", "--detach", worktree, featureSHA)

	// The fake Codex is the resolution session: it merges the base, settles
	// the conflict, commits the merge, and reports back through -o.
	codexBin := writeTool(t, "codex", `
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@example.invalid
export GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@example.invalid
git merge --no-commit origin/main >/dev/null 2>&1 || true
printf 'resolved\n' > shared.txt
git add shared.txt
git commit -q -m "Merge main into feature"
printf 'merged and resolved\n' > "$out"`)

	root := t.TempDir()
	r := &run{
		p:         &Pipeline{Git: git.New("git")},
		o:         Options{ResolveConflicts: true, RepoRoot: clone, FixTimeout: time.Minute},
		ctx:       context.Background(),
		rep:       NopReporter{},
		target:    target.Target{BranchOnly: true},
		pr:        gh.FullPR{BaseRefName: "main"},
		sessionID: "session-under-test",
		branch:    "feature",
		worktree:  worktree,
		logDir:    filepath.Join(root, "logs"),
		msgDir:    filepath.Join(root, "messages"),
		codex:     codex.Options{Bin: codexBin},
	}
	for _, d := range []string{r.logDir, r.msgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.env = envexec.Env{Worktree: worktree}

	if err := r.ensureMergeable(); err != nil {
		t.Fatal(err)
	}

	mergedSHA := gitIn(worktree, "rev-parse", "HEAD")
	if mergedSHA == featureSHA {
		t.Fatal("no merge commit was created")
	}
	if pushed := gitIn(origin, "rev-parse", "refs/heads/feature"); pushed != mergedSHA {
		t.Fatalf("origin/feature = %s, want the merge commit %s", pushed, mergedSHA)
	}
	if r.headSHA != mergedSHA {
		t.Fatalf("pinned head = %s, want %s", r.headSHA, mergedSHA)
	}
	if content, err := os.ReadFile(filepath.Join(worktree, "shared.txt")); err != nil ||
		string(content) != "resolved\n" {
		t.Fatalf("shared.txt = %q, %v", content, err)
	}

	// The branch merges cleanly now, so a second call must not start another
	// session; the fake codex would fail its merge and the test with it.
	if err := r.ensureMergeable(); err != nil {
		t.Fatal(err)
	}
	if r.conflictFixes != 1 {
		t.Fatalf("conflict sessions = %d, want 1", r.conflictFixes)
	}
}

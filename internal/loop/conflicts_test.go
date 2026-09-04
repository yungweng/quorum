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
	"github.com/yungweng/quorum/internal/review"
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

type baseUpdateRepo struct {
	t                       *testing.T
	origin, clone, worktree string
	featureSHA              string
}

func newBaseUpdateRepo(t *testing.T) *baseUpdateRepo {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	f := &baseUpdateRepo{t: t, origin: t.TempDir(), clone: t.TempDir()}
	f.git(f.origin, "init", "-q", "--bare", "-b", "main")
	f.git(f.clone, "init", "-q", "-b", "main")
	f.git(f.clone, "config", "user.name", "Example User")
	f.git(f.clone, "config", "user.email", "example@example.invalid")
	f.git(f.clone, "config", "commit.gpgSign", "false")
	f.git(f.clone, "remote", "add", "origin", f.origin)
	f.write("base.txt", "base\n")
	f.git(f.clone, "add", "base.txt")
	f.git(f.clone, "commit", "-q", "-m", "base")
	f.git(f.clone, "checkout", "-q", "-b", "feature")
	f.write("feature.txt", "feature\n")
	f.git(f.clone, "add", "feature.txt")
	f.git(f.clone, "commit", "-q", "-m", "feature")
	f.featureSHA = f.git(f.clone, "rev-parse", "HEAD")
	f.git(f.clone, "push", "-q", "origin", "main", "feature")
	f.git(f.clone, "checkout", "-q", "main")
	f.worktree = filepath.Join(t.TempDir(), "worktree")
	f.git(f.clone, "worktree", "add", "--quiet", "--detach", f.worktree, f.featureSHA)
	return f
}

func (f *baseUpdateRepo) git(dir string, args ...string) string {
	f.t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func (f *baseUpdateRepo) write(name, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.clone, name), []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *baseUpdateRepo) advanceBase() string {
	f.t.Helper()
	f.write("base.txt", "advanced base\n")
	f.git(f.clone, "commit", "-q", "-am", "advance base")
	sha := f.git(f.clone, "rev-parse", "HEAD")
	f.git(f.clone, "push", "-q", "origin", "main")
	return sha
}

func (f *baseUpdateRepo) run(reviewer Reviewer, maxIter int) *run {
	return &run{
		p:        &Pipeline{Git: git.New("git"), Review: reviewer},
		o:        Options{ResolveConflicts: true, RepoRoot: f.clone, MaxIter: maxIter},
		ctx:      context.Background(),
		rep:      NopReporter{},
		target:   target.Target{BranchOnly: true},
		pr:       gh.FullPR{BaseRefName: "main"},
		branch:   "feature",
		headSHA:  f.featureSHA,
		worktree: f.worktree,
		logDir:   f.t.TempDir(),
		env:      envexec.Env{Worktree: f.worktree},
	}
}

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
	if _, err := r.ensureBaseCurrent(); err != nil {
		t.Fatal(err)
	}
}

func TestBaseUpdateStopsWhenGitCannotStartTheMerge(t *testing.T) {
	bin := writeTool(t, "git", `
case "$*" in
  "fetch -q origin +refs/heads/main:refs/remotes/origin/main") ;;
	"merge-base --is-ancestor origin/main HEAD") exit 1 ;;
	"rev-parse HEAD") echo "head-sha" ;;
	"merge --no-ff -m Merge main into feature origin/main") echo "merge refused" >&2; exit 2 ;;
	"diff --name-only --diff-filter=U") ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac`)
	worktree := t.TempDir()
	r := &run{
		p:        &Pipeline{Git: git.New(bin)},
		o:        Options{ResolveConflicts: true, RepoRoot: t.TempDir()},
		ctx:      context.Background(),
		rep:      NopReporter{},
		pr:       gh.FullPR{BaseRefName: "main"},
		branch:   "feature",
		worktree: worktree,
		env:      envexec.Env{Worktree: worktree},
	}
	if _, err := r.ensureBaseCurrent(); err == nil || !strings.Contains(err.Error(), "merge refused") {
		t.Fatalf("base update error = %v, want the merge failure", err)
	}
}

func TestCurrentBranchNeverStartsABaseUpdate(t *testing.T) {
	bin := writeTool(t, "git", `
case "$*" in
  "fetch -q origin +refs/heads/main:refs/remotes/origin/main") ;;
	"merge-base --is-ancestor origin/main HEAD") ;;
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
	if _, err := r.ensureBaseCurrent(); err != nil {
		t.Fatal(err)
	}
	if r.conflictFixes != 0 {
		t.Fatal("a clean branch started a conflict session")
	}
}

func TestCleanBehindBranchIsUpdatedAndPushed(t *testing.T) {
	f := newBaseUpdateRepo(t)
	baseSHA := f.advanceBase()
	if _, err := f.run(nil, 0).ensureBaseCurrent(); err != nil {
		t.Fatal(err)
	}
	mergedSHA := f.git(f.worktree, "rev-parse", "HEAD")
	if mergedSHA == f.featureSHA {
		t.Fatal("the clean base update did not move HEAD")
	}
	if !strings.Contains(f.git(f.worktree, "log", "-1", "--format=%s"), "Merge") {
		t.Fatal("the clean base update did not create a merge commit")
	}
	if got := f.git(f.origin, "rev-parse", "refs/heads/feature"); got != mergedSHA {
		t.Fatalf("origin/feature = %s, want %s", got, mergedSHA)
	}
	if code := exec.Command("git", "-C", f.worktree, "merge-base", "--is-ancestor", baseSHA, "HEAD").Run(); code != nil {
		t.Fatalf("updated branch does not contain base %s: %v", baseSHA, code)
	}
}

func TestOfflineBaseUpdateDefersThePush(t *testing.T) {
	f := newBaseUpdateRepo(t)
	f.advanceBase()
	r := f.run(nil, 0)
	r.o.Offline = true

	updated, err := r.ensureBaseCurrent()
	if err != nil || !updated {
		t.Fatalf("base update = %v, %v; want an update", updated, err)
	}
	if head := f.git(f.worktree, "rev-parse", "HEAD"); head == f.featureSHA {
		t.Fatal("offline update did not move the worktree")
	}
	if head := f.git(f.origin, "rev-parse", "refs/heads/feature"); head != f.featureSHA {
		t.Fatalf("offline update pushed %s before convergence", head)
	}
}

type reviewerFunc func(context.Context, review.Options) (*review.Result, error)

func (f reviewerFunc) Run(ctx context.Context, o review.Options) (*review.Result, error) {
	return f(ctx, o)
}

func executeAcrossBaseAdvance(t *testing.T, f *baseUpdateRepo, maxIter int) (*Result, []string, error) {
	t.Helper()
	started := make(chan struct{})
	resume := make(chan struct{})
	var reviewed []string
	reviewer := reviewerFunc(func(_ context.Context, o review.Options) (*review.Result, error) {
		reviewed = append(reviewed, o.HeadSHA)
		if len(reviewed) == 1 {
			close(started)
			<-resume
		}
		return &review.Result{Findings: review.Findings{HeadSHA: o.HeadSHA}}, nil
	})
	type outcome struct {
		res *Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := f.run(reviewer, maxIter).execute()
		done <- outcome{res: res, err: err}
	}()
	<-started
	f.advanceBase()
	close(resume)
	out := <-done
	return out.res, reviewed, out.err
}

func TestBehindBranchIsUpdatedBeforeFirstReview(t *testing.T) {
	f := newBaseUpdateRepo(t)
	baseSHA := f.advanceBase()
	var reviewed string
	reviewer := reviewerFunc(func(_ context.Context, o review.Options) (*review.Result, error) {
		reviewed = o.HeadSHA
		return &review.Result{Findings: review.Findings{HeadSHA: o.HeadSHA}}, nil
	})

	res, err := f.run(reviewer, 1).execute()
	if err != nil || !res.Converged {
		t.Fatalf("execute = converged %v, %v", res.Converged, err)
	}
	if reviewed == f.featureSHA {
		t.Fatal("the first review used the stale branch head")
	}
	if code := exec.Command("git", "-C", f.worktree, "merge-base", "--is-ancestor", baseSHA, reviewed).Run(); code != nil {
		t.Fatalf("first reviewed head does not contain base %s: %v", baseSHA, code)
	}
}

func TestBaseAdvanceDuringCleanReviewStartsAnotherReview(t *testing.T) {
	f := newBaseUpdateRepo(t)
	res, reviewed, err := executeAcrossBaseAdvance(t, f, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Converged {
		t.Fatal("run did not converge")
	}
	if len(reviewed) != 2 {
		t.Fatalf("reviewed %d heads, want the original and updated heads", len(reviewed))
	}
	if reviewed[0] == reviewed[1] {
		t.Fatalf("second review reused stale head %s", reviewed[0])
	}
	baseSHA := f.git(f.origin, "rev-parse", "refs/heads/main")
	if code := exec.Command("git", "-C", f.worktree, "merge-base", "--is-ancestor", baseSHA, reviewed[1]).Run(); code != nil {
		t.Fatalf("reviewed head does not contain base %s: %v", baseSHA, code)
	}
}

func TestBaseAdvanceAtReviewLimitDoesNotReportReady(t *testing.T) {
	f := newBaseUpdateRepo(t)
	res, _, err := executeAcrossBaseAdvance(t, f, 1)
	if !errors.Is(err, ErrNotConverged) {
		t.Fatalf("execute error = %v, want ErrNotConverged", err)
	}
	if res.Converged {
		t.Fatal("run reported ready without reviewing the updated head")
	}
}

func TestConflictSessionThatCommitsNothingStopsTheRun(t *testing.T) {
	gitBin := writeTool(t, "git", `
case "$*" in
  "fetch -q origin +refs/heads/main:refs/remotes/origin/main") ;;
	"merge-base --is-ancestor origin/main HEAD") exit 1 ;;
  "rev-parse HEAD") echo "same-sha" ;;
	"merge --no-ff -m Merge main into feature origin/main") echo "CONFLICT" >&2; exit 1 ;;
	"diff --name-only --diff-filter=U") echo "shared.txt" ;;
	"status --porcelain") echo "UU shared.txt" ;;
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
		fixer:     codex.Options{Bin: codexBin},
	}
	for _, d := range []string{r.logDir, r.msgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r.env = envexec.Env{Worktree: r.worktree}

	_, err := r.ensureBaseCurrent()
	if !errors.Is(err, ErrConflicts) {
		t.Fatalf("ensureBaseCurrent = %v, want ErrConflicts", err)
	}
}

func conflictResolutionRun(t *testing.T, f *baseUpdateRepo) *run {
	t.Helper()
	f.write("feature.txt", "main\n")
	f.git(f.clone, "add", "feature.txt")
	f.git(f.clone, "commit", "-q", "-m", "conflicting base change")
	f.git(f.clone, "push", "-q", "origin", "main")
	codexBin := writeTool(t, "codex", `
out=""
prev=""
for a in "$@"; do
  if [ "$prev" = "-o" ]; then out="$a"; fi
  prev="$a"
done
printf 'resolved\n' > feature.txt
git add feature.txt
git commit -q -m "Merge main into feature"
printf 'merged and resolved\n' > "$out"`)

	root := t.TempDir()
	r := f.run(nil, 0)
	r.o.FixTimeout = time.Minute
	r.sessionID = "session-under-test"
	r.logDir = filepath.Join(root, "logs")
	r.msgDir = filepath.Join(root, "messages")
	r.fixer = codex.Options{Bin: codexBin}
	for _, d := range []string{r.logDir, r.msgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// TestConflictResolutionMergesAndPushes drives the full path against real
// repositories: a feature branch that conflicts with main, a fake Codex that
// resolves the active merge, and a bare origin the result must arrive on.
func TestConflictResolutionMergesAndPushes(t *testing.T) {
	f := newBaseUpdateRepo(t)
	r := conflictResolutionRun(t, f)
	if _, err := r.ensureBaseCurrent(); err != nil {
		t.Fatal(err)
	}

	mergedSHA := f.git(f.worktree, "rev-parse", "HEAD")
	if mergedSHA == f.featureSHA {
		t.Fatal("no merge commit was created")
	}
	if pushed := f.git(f.origin, "rev-parse", "refs/heads/feature"); pushed != mergedSHA {
		t.Fatalf("origin/feature = %s, want the merge commit %s", pushed, mergedSHA)
	}
	if r.headSHA != mergedSHA {
		t.Fatalf("pinned head = %s, want %s", r.headSHA, mergedSHA)
	}
	if content, err := os.ReadFile(filepath.Join(f.worktree, "feature.txt")); err != nil ||
		string(content) != "resolved\n" {
		t.Fatalf("feature.txt = %q, %v", content, err)
	}

	// The branch merges cleanly now, so a second call must not start another
	// session; the fake codex would fail its merge and the test with it.
	if _, err := r.ensureBaseCurrent(); err != nil {
		t.Fatal(err)
	}
	if r.conflictFixes != 1 {
		t.Fatalf("conflict sessions = %d, want 1", r.conflictFixes)
	}
}

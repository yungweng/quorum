package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/target"
)

func fakeReviewGit(t *testing.T) git.G {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
case "$*" in
  "fetch -q origin +refs/heads/main:refs/remotes/origin/main") ;;
  "rev-parse origin/main") echo "base-sha" ;;
  "fetch -q origin +refs/heads/feature/crumb-tray:refs/remotes/origin/feature/crumb-tray") ;;
  "rev-parse refs/remotes/origin/feature/crumb-tray") echo "head-sha" ;;
  worktree\ add\ --quiet\ --detach\ *) ;;
  "ls-remote origin refs/heads/main") printf 'base-sha\trefs/heads/main\n' ;;
  "ls-remote origin refs/heads/feature/crumb-tray") printf 'head-sha\trefs/heads/feature/crumb-tray\n' ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return git.New(bin)
}

func TestBranchCheckoutAndDriftUseBranchRefsNotPullZero(t *testing.T) {
	gitc := fakeReviewGit(t)
	r := &Runner{Git: gitc}
	tgt := target.Target{
		BranchOnly: true,
		PR: gh.FullPR{
			HeadRefName: "feature/crumb-tray",
			HeadRefOid:  "head-sha",
			BaseRefName: "main",
			BaseRefOid:  "base-sha",
		},
	}
	o := Options{RepoRoot: t.TempDir()}
	run := runPaths{worktree: filepath.Join(t.TempDir(), "worktree")}

	base, head, err := r.checkout(context.Background(), o, run, tgt, "main", "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if base != "base-sha" || head != "head-sha" {
		t.Fatalf("reviewed base/head = %s/%s", base, head)
	}
	if note, err := r.checkDrift(context.Background(), o, tgt, "main", base, head); err != nil || note != "" {
		t.Fatalf("drift result = %q, %v", note, err)
	}
}

func TestPinnedBranchReviewRefusesAMovedRemoteHead(t *testing.T) {
	r := &Runner{Git: fakeReviewGit(t)}
	o := Options{
		Repo:       "acme/api",
		RepoRoot:   t.TempDir(),
		Branch:     "feature/crumb-tray",
		BaseBranch: "main",
		HeadSHA:    "pipeline-head-sha",
	}
	_, _, err := r.resolveRunTarget(context.Background(), &o)
	if !errors.Is(err, ErrHeadDrifted) {
		t.Fatalf("resolveRunTarget error = %v, want ErrHeadDrifted", err)
	}
}

func TestPostVerifiedCommentRechecksRemoteHead(t *testing.T) {
	root := t.TempDir()
	gitBin := filepath.Join(root, "git")
	gitScript := `#!/bin/sh
set -eu
case "$*" in
  "ls-remote origin refs/heads/main") printf 'base-sha\trefs/heads/main\n' ;;
  "ls-remote origin refs/pull/42/head") printf 'new-head\trefs/pull/42/head\n' ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(gitBin, []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	called := filepath.Join(root, "gh-called")
	ghBin := filepath.Join(root, "gh")
	if err := os.WriteFile(ghBin, []byte("#!/bin/sh\ntouch \""+called+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Git: git.New(gitBin), GH: gh.New(ghBin)}
	tgt := target.Target{PR: gh.FullPR{Number: 42}}
	_, err := r.postVerifiedComment(context.Background(), Options{RepoRoot: root}, tgt,
		"main", "base-sha", "reviewed-head", filepath.Join(root, "comment.md"))
	if !errors.Is(err, ErrHeadDrifted) {
		t.Fatalf("post error = %v, want ErrHeadDrifted", err)
	}
	if _, statErr := os.Stat(called); !os.IsNotExist(statErr) {
		t.Fatal("comment was posted after the remote head moved")
	}
}

func TestAllowDirenvFailsClosedWhenChangedFilesCannotBeChecked(t *testing.T) {
	worktree := t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, ".envrc"), []byte("export UNSAFE=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitBin := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(gitBin, []byte(`#!/bin/sh
echo "diff failed" >&2
exit 1
`), 0o755); err != nil {
		t.Fatal(err)
	}
	direnvBin := filepath.Join(t.TempDir(), "direnv")
	if err := os.WriteFile(direnvBin, []byte(`#!/bin/sh
touch .direnv-allowed
`), 0o755); err != nil {
		t.Fatal(err)
	}

	r := &Runner{Git: git.New(gitBin)}
	err := r.allowDirenv(context.Background(), Options{AllowEnvrcChange: true},
		runPaths{worktree: worktree}, "origin/main",
		envexec.Env{Worktree: worktree, Direnv: true, DirenvBin: direnvBin}, NopReporter{})
	if err == nil || !strings.Contains(err.Error(), "check changed .envrc files") {
		t.Fatalf("allowDirenv error = %v, want changed-file check failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(worktree, ".direnv-allowed")); !os.IsNotExist(statErr) {
		t.Fatal("direnv ran after the changed-file check failed")
	}
}

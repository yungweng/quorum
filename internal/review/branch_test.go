package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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

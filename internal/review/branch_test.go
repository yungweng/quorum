package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// A local head exists only in the caller's object database: there is nothing
// to fetch and no remote for it to drift from. The fake git fails on any
// unexpected call, so this also proves the head fetch and the head ls-remote
// are really gone; only the base is fetched and drift-checked.
func TestLocalHeadCheckoutSkipsTheHeadFetchAndHeadDrift(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
case "$*" in
  "fetch -q origin +refs/heads/main:refs/remotes/origin/main") ;;
  "rev-parse origin/main") echo "base-sha" ;;
  "rev-parse local-sha^{commit}") echo "local-sha" ;;
  worktree\ add\ --quiet\ --detach\ *) ;;
  "ls-remote origin refs/heads/main") printf 'moved-base\trefs/heads/main\n' ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Git: git.New(bin)}
	tgt := target.Target{
		BranchOnly: true,
		PR: gh.FullPR{
			HeadRefName: "feature/crumb-tray",
			HeadRefOid:  "local-sha",
			BaseRefName: "main",
			BaseRefOid:  "base-sha",
		},
	}
	o := Options{RepoRoot: t.TempDir(), LocalHead: true,
		Branch: "feature/crumb-tray", HeadSHA: "local-sha", BaseBranch: "main"}
	run := runPaths{worktree: filepath.Join(t.TempDir(), "worktree")}

	base, head, err := r.checkout(context.Background(), o, run, tgt, "main", "origin/main")
	if err != nil {
		t.Fatal(err)
	}
	if base != "base-sha" || head != "local-sha" {
		t.Fatalf("reviewed base/head = %s/%s", base, head)
	}

	note, err := r.checkDrift(context.Background(), o, tgt, "main", "base-sha", "local-sha")
	if err != nil {
		t.Fatalf("checkDrift: %v", err)
	}
	if !strings.Contains(note, "Base branch moved") {
		t.Fatalf("a moved base did not surface as a note: %q", note)
	}
}

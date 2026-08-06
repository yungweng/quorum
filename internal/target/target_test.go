package target

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
)

func fakeTool(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func branchGit(t *testing.T, dirty bool, localSHA string) git.G {
	t.Helper()
	status := ""
	if dirty {
		status = " M internal/widget.go"
	}
	bin := fakeTool(t, "git", `
case "$*" in
  "rev-parse --abbrev-ref HEAD") echo "feature/crumb-tray" ;;
  "ls-remote origin refs/heads/feature/crumb-tray") printf 'head-sha\trefs/heads/feature/crumb-tray\n' ;;
  "ls-remote origin refs/heads/main") printf 'base-sha\trefs/heads/main\n' ;;
  "status --porcelain") echo "`+status+`" ;;
  "rev-parse HEAD") echo "`+localSHA+`" ;;
  "fetch -q origin +refs/heads/feature/crumb-tray:refs/remotes/origin/feature/crumb-tray") : ;;
  "merge-base --is-ancestor local-sha head-sha") exit 1 ;;
  "merge-base --is-ancestor behind-sha head-sha") exit 0 ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac`)
	return git.New(bin)
}

func branchGH(t *testing.T, prJSON string) *gh.Client {
	t.Helper()
	view := `echo '` + prJSON + `'`
	if prJSON == "" {
		view = `echo 'no pull requests found for branch "example-user:feature/crumb-tray"' >&2; exit 1`
	}
	bin := fakeTool(t, "gh", `
case "$*" in
  "pr view --json "*) `+view+` ;;
  "repo view --json defaultBranchRef -q .defaultBranchRef.name") echo "main" ;;
  *) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac`)
	return gh.New(bin)
}

func TestResolveLocalNeverAsksForThePR(t *testing.T) {
	// The gh fake fails on any `pr view`: a local run must leave the open PR
	// alone, and the only way to guarantee that is to never even look it up.
	bin := fakeTool(t, "gh", `
case "$*" in
  "repo view --json defaultBranchRef -q .defaultBranchRef.name") echo "main" ;;
  *) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac`)
	got, err := ResolveLocal(context.Background(), gh.New(bin),
		branchGit(t, false, "head-sha"), t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.BranchOnly {
		t.Fatal("a local run did not resolve to a branch-only target")
	}
	if got.PR.HeadRefName != "feature/crumb-tray" || got.PR.BaseRefName != "main" {
		t.Fatalf("target = %+v", got.PR)
	}
}

func TestResolveLocalStillRefusesADirtyCheckout(t *testing.T) {
	bin := fakeTool(t, "gh", `
case "$*" in
  "repo view --json defaultBranchRef -q .defaultBranchRef.name") echo "main" ;;
  *) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac`)
	_, err := ResolveLocal(context.Background(), gh.New(bin),
		branchGit(t, true, "head-sha"), t.TempDir(), "")
	if err == nil || !strings.Contains(err.Error(), "uncommitted changes") {
		t.Fatalf("dirty local checkout resolved anyway: %v", err)
	}
}

func TestResolveFallsBackToAPushedBranchWithoutAnOpenPR(t *testing.T) {
	got, err := Resolve(context.Background(), branchGH(t, ""),
		branchGit(t, false, "head-sha"), t.TempDir(), 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.BranchOnly {
		t.Fatal("target was not marked as branch-only")
	}
	if got.PR.Number != 0 || got.PR.HeadRefName != "feature/crumb-tray" ||
		got.PR.HeadRefOid != "head-sha" || got.PR.BaseRefName != "main" ||
		got.PR.BaseRefOid != "base-sha" {
		t.Fatalf("branch metadata = %+v", got.PR)
	}
}

func TestResolveKeepsUsingTheCurrentBranchPRWhenOneExists(t *testing.T) {
	got, err := Resolve(context.Background(), branchGH(t,
		`{"number":42,"title":"Improve crumb tray","state":"OPEN","headRefName":"feature/crumb-tray","headRefOid":"head-sha","baseRefName":"main","baseRefOid":"base-sha"}`),
		branchGit(t, false, "head-sha"), t.TempDir(), 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.BranchOnly || got.PR.Number != 42 || got.PR.Title != "Improve crumb tray" {
		t.Fatalf("PR target = %+v", got)
	}
}

func TestResolveFallsBackWhenTheAssociatedPRIsClosed(t *testing.T) {
	prJSON := `{"number":42,"title":"Old crumb tray","state":"CLOSED","headRefName":"feature/crumb-tray","headRefOid":"head-sha","baseRefName":"main","baseRefOid":"base-sha"}`
	got, err := Resolve(context.Background(), branchGH(t, prJSON),
		branchGit(t, false, "head-sha"), t.TempDir(), 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.BranchOnly || got.PR.Number != 0 {
		t.Fatalf("closed PR was selected: %+v", got)
	}
}

func TestResolveDoesNotSearchForSameNamedForkPRs(t *testing.T) {
	got, err := Resolve(context.Background(), branchGH(t, ""),
		branchGit(t, false, "head-sha"), t.TempDir(), 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.BranchOnly || got.PR.Number != 0 || got.PR.HeadRefOid != "head-sha" {
		t.Fatalf("unrelated PR was selected: %+v", got)
	}
}

func TestResolveFindsAForkPRBeforeRequiringAnOriginBranch(t *testing.T) {
	gitc := git.New(fakeTool(t, "git", `
case "$*" in
  "rev-parse --abbrev-ref HEAD") echo "feature/crumb-tray" ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac`))
	prJSON := `{"number":42,"title":"Fork change","state":"OPEN","isCrossRepository":true,"headRefName":"feature/crumb-tray","headRefOid":"fork-head-sha","baseRefName":"main","baseRefOid":"base-sha"}`

	got, err := Resolve(context.Background(), branchGH(t, prJSON),
		gitc, t.TempDir(), 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.BranchOnly || got.PR.Number != 42 || !got.PR.IsCrossRepository {
		t.Fatalf("fork PR target = %+v", got)
	}
}

func TestResolveRefusesDirtyOrUnpushedLocalWork(t *testing.T) {
	for name, gitc := range map[string]git.G{
		"dirty":    branchGit(t, true, "head-sha"),
		"unpushed": branchGit(t, false, "local-sha"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Resolve(context.Background(), branchGH(t, ""),
				gitc, t.TempDir(), 0, "", "")
			if err == nil {
				t.Fatal("unsafe branch target was accepted")
			}
			if !strings.Contains(err.Error(), map[string]string{
				"dirty": "uncommitted changes", "unpushed": "differs from origin",
			}[name]) {
				t.Fatalf("error = %q", err)
			}
		})
	}
}

func TestResolveProceedsWhenTheLocalBranchIsMerelyBehind(t *testing.T) {
	got, err := Resolve(context.Background(), branchGH(t, ""),
		branchGit(t, false, "behind-sha"), t.TempDir(), 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.BranchOnly || got.PR.HeadRefOid != "head-sha" {
		t.Fatalf("behind checkout resolved to %+v", got.PR)
	}
}

// The remote head comes from ls-remote and may not exist locally at all. Run
// against real git: origin advanced after the last fetch, so without an
// explicit fetch the ancestor check has nothing to compare against and dies
// with exit status 128 instead of recognising a merely-behind checkout.
func TestResolveLocalFetchesTheRemoteHeadForTheAncestorCheck(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	origin := filepath.Join(base, "origin.git")
	work := filepath.Join(base, "work")
	clone := filepath.Join(base, "clone")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}

	run(base, "init", "-q", "--bare", "-b", "main", origin)
	run(base, "init", "-q", "-b", "main", work)
	run(work, "commit", "-q", "--allow-empty", "-m", "base")
	run(work, "checkout", "-q", "-b", "feature")
	run(work, "commit", "-q", "--allow-empty", "-m", "feature work")
	run(work, "remote", "add", "origin", origin)
	run(work, "push", "-q", "origin", "main", "feature")
	run(base, "clone", "-q", origin, clone)
	run(clone, "checkout", "-q", "feature")
	run(work, "commit", "-q", "--allow-empty", "-m", "pushed elsewhere")
	run(work, "push", "-q", "origin", "feature")
	remoteHead := run(work, "rev-parse", "HEAD")

	// gh must never run: the base branch is given, and a local run never asks
	// for the PR.
	got, err := ResolveLocal(context.Background(), gh.New(filepath.Join(base, "gh-must-not-run")),
		git.New("git"), clone, "main")
	if err != nil {
		t.Fatalf("a checkout merely behind origin was refused: %v", err)
	}
	if !got.BranchOnly || got.PR.HeadRefOid != remoteHead {
		t.Fatalf("behind checkout resolved to %+v, want head %s", got.PR, remoteHead)
	}
}

func TestResolveRefusesAmbiguousCurrentBranchPRs(t *testing.T) {
	ghc := gh.New(fakeTool(t, "gh", `
case "$*" in
  "pr view --json "*) echo "multiple pull requests found for branch" >&2; exit 1 ;;
  *) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac`))
	_, err := Resolve(context.Background(), ghc, branchGit(t, false, "head-sha"),
		t.TempDir(), 0, "", "")
	if err == nil || !strings.Contains(err.Error(), "multiple pull requests") {
		t.Fatalf("error = %v", err)
	}
}

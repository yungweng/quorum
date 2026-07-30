package target

import (
	"context"
	"os"
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
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac`)
	return git.New(bin)
}

func branchGH(t *testing.T, listJSON string) *gh.Client {
	t.Helper()
	bin := fakeTool(t, "gh", `
case "$*" in
  "pr list --head feature/crumb-tray --state open --limit 100 --json "*) echo '`+listJSON+`' ;;
  "repo view --json defaultBranchRef -q .defaultBranchRef.name") echo "main" ;;
  *) echo "unexpected gh call: $*" >&2; exit 1 ;;
esac`)
	return gh.New(bin)
}

func TestResolveFallsBackToAPushedBranchWithoutAnOpenPR(t *testing.T) {
	got, err := Resolve(context.Background(), branchGH(t, "[]"),
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
		`[{"number":42,"title":"Improve crumb tray","state":"OPEN","headRefName":"feature/crumb-tray","headRefOid":"head-sha","baseRefName":"main","baseRefOid":"base-sha"}]`),
		branchGit(t, false, "head-sha"), t.TempDir(), 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.BranchOnly || got.PR.Number != 42 || got.PR.Title != "Improve crumb tray" {
		t.Fatalf("PR target = %+v", got)
	}
}

func TestResolveIgnoresAnUnrelatedPRWithTheSameBranchName(t *testing.T) {
	prJSON := `{"number":42,"title":"Fork change","state":"OPEN","isCrossRepository":true,"headRefName":"feature/crumb-tray","headRefOid":"other-sha","baseRefName":"main","baseRefOid":"base-sha"}`
	got, err := Resolve(context.Background(),
		branchGH(t, `[`+prJSON+`]`),
		branchGit(t, false, "head-sha"), t.TempDir(), 0, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if !got.BranchOnly || got.PR.Number != 0 || got.PR.HeadRefOid != "head-sha" {
		t.Fatalf("unrelated PR was selected: %+v", got)
	}
}

func TestResolveRefusesAPRWhoseHeadDiffersFromTheCheckout(t *testing.T) {
	prJSON := `{"number":42,"title":"Old change","state":"OPEN","headRefName":"feature/crumb-tray","headRefOid":"other-sha","baseRefName":"main","baseRefOid":"base-sha"}`
	_, err := Resolve(context.Background(),
		branchGH(t, `[`+prJSON+`]`),
		branchGit(t, false, "head-sha"), t.TempDir(), 0, "", "")
	if err == nil || !strings.Contains(err.Error(), "differs from the checked-out branch") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveFindsAForkPRBeforeRequiringAnOriginBranch(t *testing.T) {
	gitc := git.New(fakeTool(t, "git", `
case "$*" in
  "rev-parse --abbrev-ref HEAD") echo "feature/crumb-tray" ;;
  "rev-parse HEAD") echo "fork-head-sha" ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac`))
	prJSON := `{"number":42,"title":"Fork change","state":"OPEN","isCrossRepository":true,"headRefName":"feature/crumb-tray","headRefOid":"fork-head-sha","baseRefName":"main","baseRefOid":"base-sha"}`

	got, err := Resolve(context.Background(), branchGH(t, `[`+prJSON+`]`),
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
			_, err := Resolve(context.Background(), branchGH(t, "[]"),
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

func TestResolveRefusesAmbiguousCurrentBranchPRs(t *testing.T) {
	_, err := Resolve(context.Background(), branchGH(t, `[
		{"number":41,"title":"First crumb tray","state":"OPEN","headRefName":"feature/crumb-tray","headRefOid":"head-sha","baseRefName":"main","baseRefOid":"base-sha"},
		{"number":42,"title":"Second crumb tray","state":"OPEN","headRefName":"feature/crumb-tray","headRefOid":"head-sha","baseRefName":"main","baseRefOid":"base-sha"}
	]`),
		branchGit(t, false, "head-sha"), t.TempDir(), 0, "", "")
	if err == nil || !strings.Contains(err.Error(), "multiple open pull requests") {
		t.Fatalf("error = %v", err)
	}
}

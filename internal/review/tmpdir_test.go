package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/runtmp"
)

// A failed run keeps its worktree for --resume-run, but its private TMPDIR
// has to go on every exit path: leaked nix profile trees in it are what fill
// a tmpfs /tmp's inode table.
func TestFailedRunRemovesItsPrivateTmpDir(t *testing.T) {
	tmpRoot := t.TempDir()
	t.Setenv("TMPDIR", tmpRoot)

	root := t.TempDir()
	if err := writeRunTarget(filepath.Join(root, "target.json"), runTarget{
		Schema: runTargetSchema, Repo: "acme/api",
		Branch: "feature/crumb-tray", BaseBranch: "main", BranchOnly: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "worktree"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "worktree", ".git"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	// git fails the checkout, after the private TMPDIR exists. It records
	// what the temp root held at that moment.
	seen := filepath.Join(t.TempDir(), "seen")
	gitBin := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
case "$*" in
  "ls-remote origin refs/heads/main") printf 'base-sha\trefs/heads/main\n'; exit 0 ;;
  "ls-remote origin refs/heads/feature/crumb-tray") printf 'head-sha\trefs/heads/feature/crumb-tray\n'; exit 0 ;;
esac
ls "` + tmpRoot + `" > "` + seen + `"
exit 1
`
	if err := os.WriteFile(gitBin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	cache := t.TempDir()
	o := Options{
		Repo: "acme/api", RepoRoot: t.TempDir(), ResumeRun: root,
		RunsDir: filepath.Join(cache, "reviews"), DepsDir: filepath.Join(cache, "deps"),
	}

	_, runErr := (&Runner{Git: git.New(gitBin)}).Run(context.Background(), o)
	if runErr == nil {
		t.Fatal("run succeeded with a failing git")
	}

	during, err := os.ReadFile(seen)
	if err != nil {
		t.Fatalf("git never ran: %v", err)
	}
	if !strings.Contains(string(during), runtmp.Prefix) {
		t.Fatalf("no private TMPDIR existed while the run was active: %q (run error: %v)", during, runErr)
	}
	left, err := os.ReadDir(tmpRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range left {
		if strings.HasPrefix(e.Name(), runtmp.Prefix) {
			t.Fatalf("failed run left %s behind", e.Name())
		}
	}
}

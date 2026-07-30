package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/target"
)

func TestBranchPushBarrierReadsTheRemoteBranchWithoutAPR(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "git")
	script := `#!/bin/sh
set -eu
case "$*" in
  "rev-parse HEAD") echo "head-sha" ;;
  "push -q origin HEAD:refs/heads/feature/crumb-tray") ;;
  "ls-remote origin refs/heads/feature/crumb-tray") printf 'head-sha\trefs/heads/feature/crumb-tray\n' ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	r := &run{
		p:        &Pipeline{Git: git.New(bin)},
		o:        Options{RepoRoot: t.TempDir()},
		ctx:      context.Background(),
		rep:      NopReporter{},
		target:   target.Target{BranchOnly: true},
		branch:   "feature/crumb-tray",
		worktree: t.TempDir(),
		logDir:   logDir,
	}
	if err := r.pushBranch(); err != nil {
		t.Fatal(err)
	}
}

func TestBranchDirenvRequiresAnExplicitOverrideForTargetChanges(t *testing.T) {
	for _, allow := range []bool{false, true} {
		t.Run(map[bool]string{false: "refused", true: "allowed"}[allow], func(t *testing.T) {
			worktree := t.TempDir()
			if err := os.WriteFile(filepath.Join(worktree, ".envrc"), []byte("export UNSAFE=1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitBin := filepath.Join(t.TempDir(), "git")
			gitScript := `#!/bin/sh
set -eu
case "$*" in
  "fetch -q origin +refs/heads/main:refs/remotes/origin/main") ;;
  "diff --name-only origin/main...HEAD -- .envrc :(glob)**/.envrc") echo ".envrc" ;;
  *) echo "unexpected git call: $*" >&2; exit 1 ;;
esac
`
			if err := os.WriteFile(gitBin, []byte(gitScript), 0o755); err != nil {
				t.Fatal(err)
			}
			direnvBin := filepath.Join(t.TempDir(), "direnv")
			direnvScript := `#!/bin/sh
set -eu
test "$*" = "allow"
touch .direnv-allowed
`
			if err := os.WriteFile(direnvBin, []byte(direnvScript), 0o755); err != nil {
				t.Fatal(err)
			}

			r := &run{
				p: &Pipeline{Git: git.New(gitBin)},
				o: Options{
					RepoRoot:         t.TempDir(),
					AllowEnvrcChange: allow,
				},
				ctx:      context.Background(),
				rep:      NopReporter{},
				target:   target.Target{BranchOnly: true},
				pr:       gh.FullPR{BaseRefName: "main"},
				worktree: worktree,
				env: envexec.Env{
					Worktree:  worktree,
					Direnv:    true,
					DirenvBin: direnvBin,
				},
			}
			err := r.setupDirenv()
			if !allow {
				if !errors.Is(err, review.ErrEnvrcChanged) {
					t.Fatalf("setupDirenv error = %v, want ErrEnvrcChanged", err)
				}
				if _, statErr := os.Stat(filepath.Join(worktree, ".direnv-allowed")); !os.IsNotExist(statErr) {
					t.Fatal("direnv ran before the target .envrc was approved")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(worktree, ".direnv-allowed")); err != nil {
				t.Fatal("direnv did not run after the explicit override")
			}
		})
	}
}

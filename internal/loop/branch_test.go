package loop

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yungweng/quorum/internal/git"
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

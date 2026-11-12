package rundir

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/git"
)

func TestAllowEnvrcFailsClosedWhenChangedFilesCannotBeChecked(t *testing.T) {
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

	_, err := AllowEnvrc(context.Background(), git.New(gitBin),
		envexec.Env{Worktree: worktree, Direnv: true, DirenvBin: direnvBin}, "origin/main", true, nil, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "check changed .envrc files") {
		t.Fatalf("AllowEnvrc error = %v, want changed-file check failure", err)
	}
	if _, statErr := os.Stat(filepath.Join(worktree, ".direnv-allowed")); !os.IsNotExist(statErr) {
		t.Fatal("direnv ran after the changed-file check failed")
	}
}

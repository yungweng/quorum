package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/git"
)

func TestDirtyWorktreeErrorPreservesStatusEvidence(t *testing.T) {
	dir := t.TempDir()
	gitBin := filepath.Join(dir, "git")
	gitScript := `#!/bin/sh
set -eu
if [ "$*" = "status --porcelain" ]; then
  printf '%s\n' '?? frontend/tmp/pr42/screenshot.png'
  exit 0
fi
echo "unexpected git call: $*" >&2
exit 1
`
	if err := os.WriteFile(gitBin, []byte(gitScript), 0o755); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(dir, "worktree")
	logDir := filepath.Join(dir, "logs")
	for _, path := range []string{worktree, logDir} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r := &run{
		p:        &Pipeline{Git: git.New(gitBin)},
		ctx:      context.Background(),
		worktree: worktree,
		logDir:   logDir,
	}

	err := r.requireCleanWorktree("fix-round-1")
	if err == nil {
		t.Fatal("dirty worktree was accepted")
	}
	logPath := filepath.Join(logDir, "fix-round-1-dirty.log")
	for _, want := range []string{"?? frontend/tmp/pr42/screenshot.png", worktree, logPath} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("dirty error is missing %q: %v", want, err)
		}
	}
	status, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if got, want := string(status), "?? frontend/tmp/pr42/screenshot.png\n"; got != want {
		t.Fatalf("dirty status log = %q, want %q", got, want)
	}
}

func TestDirtyStatusPreviewIsBounded(t *testing.T) {
	var paths []string
	for i := 0; i < dirtyStatusPreviewLimit+2; i++ {
		paths = append(paths, "?? generated/file-"+string(rune('a'+i)))
	}
	got := dirtyStatusPreview(strings.Join(paths, "\n"))
	if !strings.Contains(got, paths[dirtyStatusPreviewLimit-1]) ||
		strings.Contains(got, paths[dirtyStatusPreviewLimit]) ||
		!strings.Contains(got, "... and 2 more path(s)") {
		t.Fatalf("dirty status preview was not bounded: %q", got)
	}
}

func TestCleanupKeepsOnlyFailedOrRequestedWorktrees(t *testing.T) {
	for _, test := range []struct {
		name         string
		succeeded    bool
		keepWorktree bool
		wantRemove   bool
	}{
		{name: "successful default", succeeded: true, wantRemove: true},
		{name: "successful explicit keep", succeeded: true, keepWorktree: true},
		{name: "failed default", succeeded: false},
		{name: "failed explicit keep", succeeded: false, keepWorktree: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			calls := filepath.Join(dir, "git-calls")
			gitBin := filepath.Join(dir, "git")
			gitScript := "#!/bin/sh\nset -eu\nprintf '%s\\n' \"$*\" >> \"$TEST_GIT_CALLS\"\n"
			if err := os.WriteFile(gitBin, []byte(gitScript), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("TEST_GIT_CALLS", calls)
			worktree := filepath.Join(dir, "worktree")
			if err := os.Mkdir(worktree, 0o755); err != nil {
				t.Fatal(err)
			}
			released := false
			r := &run{
				p:            &Pipeline{Git: git.New(gitBin)},
				o:            Options{RepoRoot: dir, KeepWorktree: test.keepWorktree},
				ctx:          context.Background(),
				worktree:     worktree,
				releaseClaim: func() { released = true },
			}

			r.cleanup(test.succeeded)
			if !released {
				t.Fatal("cleanup did not release the run claim")
			}
			call, err := os.ReadFile(calls)
			removed := err == nil && strings.Contains(string(call), "worktree remove "+worktree+" --force")
			if removed != test.wantRemove {
				t.Fatalf("worktree removal = %v, want %v; calls = %q, read error = %v",
					removed, test.wantRemove, call, err)
			}
		})
	}
}

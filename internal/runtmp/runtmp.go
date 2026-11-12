// Package runtmp gives every run a private TMPDIR and removes it afterwards.
//
// Everything a run starts writes its scratch files to TMPDIR, and some of it
// never cleans up: `nix profile install`, which devbox runs in every fresh
// worktree, leaves its profile tree (thousands of files) behind on each call,
// and agent test runs leave go-build and multipart directories. On a tmpfs
// /tmp with an inode limit, a few dozen reviews are enough to make every
// write on the machine fail with "no space left on device" while df still
// shows free space. With a private TMPDIR whatever a run leaks goes away
// with the run.
//
// The directory sits directly in the system temp directory, not in the run
// directory: Unix socket paths are limited to about 100 bytes, and a path
// under the cache would leave tools that create sockets in TMPDIR no room.
package runtmp

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/yungweng/quorum/internal/cachefs"
	"github.com/yungweng/quorum/internal/proc"
)

const Prefix = "quorum-"

// New creates and claims a private temp directory. The returned function
// removes it; it is safe to call more than once. Run it on every exit path:
// a directory left behind by a killed process stays until the next Sweep.
func New() (string, func(), error) {
	dir, err := os.MkdirTemp("", Prefix)
	if err != nil {
		return "", nil, err
	}
	release, err := proc.Claim(dir)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	var once sync.Once
	return dir, func() {
		once.Do(func() {
			// Remove while still holding the claim, so a concurrent Sweep
			// cannot mistake the half-removed directory for an orphan.
			_ = cachefs.RemoveAll(dir)
			release()
		})
	}, nil
}

// Sweep removes private temp directories whose run ended without removing
// them, for example after a SIGKILL. It reports how many it removed.
//
// Only a directory holding an unlocked claim file counts as an orphan. One
// without the file is either between creation and claim or not quorum's, and
// is left alone either way.
func Sweep() int {
	return sweep(os.TempDir())
}

func sweep(root string) int {
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), Prefix) {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Lstat(filepath.Join(dir, proc.ClaimFile)); err != nil || proc.Claimed(dir) {
			continue
		}
		if cachefs.RemoveAll(dir) == nil {
			removed++
		}
	}
	return removed
}

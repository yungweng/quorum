// Package rundir owns the directory a review or fix run works in: its name,
// its layout, the claim that marks it live, the sweep that removes old ones,
// and the isolated environment its worktree runs commands in, including the
// refusal to allow an .envrc the target changed. Reviews, fix loops and cache
// collection all go through it, so the three can never disagree about which
// directory belongs to a live run.
package rundir

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/yungweng/quorum/internal/cachefs"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/proc"
)

// retention is how long a finished run directory stays around after anything
// last wrote to it.
const retention = 7 * 24 * time.Hour

// Worktree is where a run's checkout lives. It is the bulk of a run directory
// and the part a successful run removes.
func Worktree(root string) string { return filepath.Join(root, "worktree") }

// CacheDir is the directory every run root and the dependency cache share.
func CacheDir(depsDir string) string { return filepath.Dir(depsDir) }

// LockCache takes the lock that serializes run startup against cache
// collection. Collection holds it from its liveness check through dependency
// eviction, and a run creates and claims its directory while holding it, so
// collection happens wholly before a startup or sees that run as live.
func LockCache(depsDir string) (func(), error) {
	return proc.LockDir(CacheDir(depsDir))
}

// Start creates the run directory with its subdirectories and claims it, all
// under the cache lock. sweep runs under the same lock once the claim exists,
// which is the one moment old runs can be removed without racing a startup.
// The returned function releases the claim.
func Start(depsDir, root string, subdirs []string, sweep func()) (release func(), err error) {
	unlock, err := LockCache(depsDir)
	if err != nil {
		return nil, err
	}
	defer unlock()
	for _, d := range subdirs {
		if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
			return nil, err
		}
	}
	release, err = proc.Claim(root)
	if err != nil {
		return nil, err
	}
	sweep()
	return release, nil
}

// Sweep removes the run directories under runsDir that nothing has written to
// within retention, and clears worktree registrations left behind by killed
// runs. Claimed directories are never touched.
//
// live reports whether a run other than current may still be working there. It
// is conservative: an unreadable root may hide a live claim, while a root that
// has never been created cannot.
func Sweep(ctx context.Context, g git.G, repoRoot, runsDir, current string) (removed int, live bool) {
	defer g.WorktreePrune(ctx, repoRoot)
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return 0, !os.IsNotExist(err)
	}
	cutoff := time.Now().Add(-retention)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(runsDir, e.Name())
		if proc.Claimed(dir) {
			if dir != current {
				live = true
			}
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		// Unregister before deleting, or git keeps a dangling worktree
		// entry that later prunes have to clean up.
		g.WorktreeRemove(ctx, repoRoot, Worktree(dir))
		if cachefs.RemoveAll(dir) == nil {
			removed++
		}
	}
	return removed, live
}

// AnyClaimed reports whether a run under runsDir may be live, with the same
// conservative reading of an unreadable root as Sweep.
func AnyClaimed(runsDir string) bool {
	entries, err := os.ReadDir(runsDir)
	if err != nil {
		return !os.IsNotExist(err)
	}
	for _, e := range entries {
		if e.IsDir() && proc.Claimed(filepath.Join(runsDir, e.Name())) {
			return true
		}
	}
	return false
}

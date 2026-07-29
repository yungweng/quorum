package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/deps"
	"github.com/yungweng/quorum/internal/proc"
	"github.com/yungweng/quorum/internal/runner"
	"github.com/yungweng/quorum/internal/state"
	"github.com/yungweng/quorum/internal/ui"
)

// cmdGC trims the cache to its budget.
func (a *app) cmdGC(args []string) int {
	dry := false
	for _, arg := range args {
		if arg == "--dry-run" || arg == "-n" {
			dry = true
		}
	}
	freed, removed, err := a.collect(dry)
	if err != nil {
		return a.die("cache collection: %v", err)
	}
	if freed > 0 || removed > 0 {
		verb := "removed"
		if dry {
			verb = "would remove"
		}
		a.out.Printf("%s %d run director%s, %s\n", verb, removed, directoryPlural(removed), ui.Bytes(freed))
		return 0
	}
	size, limit := a.cacheSize(), a.budgetBytes()
	switch {
	case limit <= 0:
		a.out.Printf("cache is %s, no budget set\n", ui.Bytes(size))
	case size > limit:
		// Everything left is spoken for, so saying "within budget" would be a lie.
		a.out.Printf("cache is %s, over its %s budget, but the rest belongs to a run in flight\n",
			ui.Bytes(size), ui.Bytes(limit))
	default:
		a.out.Printf("cache is %s, within its %s budget\n", ui.Bytes(size), ui.Bytes(limit))
	}
	return 0
}

// directoryPlural is the tail of "director-y"/"director-ies".
func directoryPlural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// budgetBytes is the cache budget in bytes, or 0 when none is set.
func (a *app) budgetBytes() int64 {
	return int64(a.cfg.CacheBudgetGB * 1024 * 1024 * 1024)
}

// cacheSizeTTL is how long one command reuses a measurement. Dependency trees
// can contain tens of thousands of files, so collection and its final report
// must not walk them twice.
const cacheSizeTTL = time.Minute

// cacheSize is everything the budget covers: both run caches and the shared
// dependency trees. The managed clones are not in it, because their size is
// bounded by how many repositories you review rather than by how often.
func (a *app) cacheSize() int64 {
	if time.Since(a.cacheAt) > cacheSizeTTL {
		a.setCacheSize(a.measureCache())
	}
	return a.cacheBytes
}

func (a *app) setCacheSize(n int64) {
	a.cacheBytes, a.cacheAt = n, time.Now()
}

// measureCache walks everything the budget covers.
func (a *app) measureCache() int64 {
	roots := []string{a.p.ReviewRuns, a.p.BabysitRuns, a.p.DepsCache}
	sizes := make(chan int64, len(roots))
	for _, root := range roots {
		go func() { sizes <- dirSize(root) }()
	}
	var total int64
	for range roots {
		total += <-sizes
	}
	return total
}

// dirSize adds up a directory tree, ignoring anything it cannot read.
func dirSize(root string) int64 {
	var total int64
	filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total
}

// runEntry is one run directory, with its size split into the worktree and
// everything else because the two are collected in different rounds.
type runEntry struct {
	path     string
	mod      time.Time
	live     bool
	worktree int64
	output   int64
}

// legacyRunGrace matches the ordinary run retention. Runs created before
// claims existed have no other liveness signal when started from a terminal,
// so recent claimless directories stay protected during the upgrade window.
const legacyRunGrace = 7 * 24 * time.Hour

// runDirs lists every review and babysit run directory, oldest first.
func (a *app) runDirs() []runEntry {
	legacyLive, protectAllLegacy := a.legacyLiveRuns()
	legacyCutoff := time.Now().Add(-legacyRunGrace)
	var runs []runEntry
	for _, root := range []string{a.p.ReviewRuns, a.p.BabysitRuns} {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			path := filepath.Join(root, e.Name())
			worktree, output := runSizes(path)
			_, claimErr := os.Stat(filepath.Join(path, proc.ClaimFile))
			recentLegacy := os.IsNotExist(claimErr) &&
				!info.ModTime().Before(legacyCutoff)
			live := proc.Claimed(path) || recentLegacy || protectAllLegacy ||
				legacyLive[filepath.Clean(path)]
			runs = append(runs, runEntry{
				path: path, mod: info.ModTime(), live: live,
				worktree: worktree, output: output,
			})
		}
	}
	slices.SortFunc(runs, func(a, b runEntry) int { return a.mod.Compare(b.mod) })
	return runs
}

// legacyLiveRuns preserves the marker/state liveness contract used before run
// claims existed. It can be removed after upgrades from v1.0.1 no longer need
// to coexist with reviews started by that version.
func (a *app) legacyLiveRuns() (map[string]bool, bool) {
	markers := runner.Live(a.p.RunningDir)
	if len(markers) == 0 {
		return nil, false
	}
	file, err := state.Read(a.p.StateFile)
	if err != nil {
		// A live legacy process with unreadable state cannot be mapped safely.
		// Preserve all runs until its marker goes away.
		return nil, true
	}
	live := make(map[string]bool, len(markers))
	protectAll := false
	for _, marker := range markers {
		rec, ok := file.PRs[marker.Key]
		if !ok || rec.RunDir == "" {
			protectAll = true
			continue
		}
		path := filepath.Clean(rec.RunDir)
		live[path] = true
	}
	return live, protectAll
}

// runSizes splits a run directory into its worktree and everything else, in one
// walk. Two calls to dirSize would descend the worktree twice, and the worktree
// is nearly every file a run has.
func runSizes(dir string) (worktree, output int64) {
	prefix := filepath.Join(dir, "worktree") + string(filepath.Separator)
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // an unreadable subtree is not worth failing over
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil //nolint:nilerr // symlinks point at the shared cache, counted there
		}
		if strings.HasPrefix(path, prefix) {
			worktree += info.Size()
		} else {
			output += info.Size()
		}
		return nil
	})
	return worktree, output
}

// collect trims the cache to its budget.
//
// Below the budget it does nothing on purpose. A worktree that outlived its run
// belongs to a review that failed, and --resume-run needs it; clearing those on
// sight would take the resume with it, which is what the seven-day sweep at the
// start of every run is for.
//
// Above the budget it works in rounds, cheapest to replace first: the worktrees
// of runs that are over, which are one checkout away from being back; then
// whole run directories, oldest first, which takes their output with them; and
// only then shared dependency trees, which cost a full install to rebuild.
func (a *app) collect(dry bool) (freed int64, removed int, err error) {
	limit := a.budgetBytes()
	if limit <= 0 {
		return 0, 0, nil
	}
	cacheRoot := filepath.Dir(a.p.DepsCache)
	if _, err := os.Stat(cacheRoot); err != nil {
		if os.IsNotExist(err) {
			if !dry {
				a.setCacheSize(0)
			}
			return 0, 0, nil
		}
		return 0, 0, err
	}
	// Run startup takes the same lock while publishing its claim. Once this
	// holds it, no run can appear between the liveness check and dependency
	// eviction, and any run that claimed first is visible below.
	unlock, err := proc.LockDir(cacheRoot)
	if err != nil {
		return 0, 0, err
	}
	defer unlock()

	// Measured, not the remembered value: this is about to delete things.
	a.setCacheSize(a.measureCache())
	total := a.cacheBytes
	if total <= limit {
		return 0, 0, nil
	}
	// What survives is the new total, so the next reader does not walk again.
	// A dry run leaves the disk alone, so it must leave the number alone too.
	if !dry {
		defer func() { a.setCacheSize(total) }()
	}

	runs := a.runDirs()
	removeAll := os.RemoveAll
	if a.removeAll != nil {
		removeAll = a.removeAll
	}
	drop := func(path string, size int64) error {
		if !dry {
			if err := removeAll(path); err != nil {
				return fmt.Errorf("remove %s: %w", path, err)
			}
		}
		total -= size
		freed += size
		return nil
	}

	for _, r := range runs {
		if total <= limit {
			return freed, removed, nil
		}
		if r.live {
			continue
		}
		if err := drop(filepath.Join(r.path, "worktree"), r.worktree); err != nil {
			return freed, removed, err
		}
	}
	// Only the output is still there to free: the round above ends by returning,
	// so reaching this point means it dropped every collectable worktree.
	for _, r := range runs {
		if total <= limit {
			return freed, removed, nil
		}
		if r.live {
			continue
		}
		if err := drop(r.path, r.output); err != nil {
			return freed, removed, err
		}
		removed++
	}
	// A run that is still going owns its worktree, and through the symlinks in
	// that worktree it owns shared trees this cannot tell apart from the rest.
	if slices.ContainsFunc(runs, func(r runEntry) bool { return r.live }) {
		return freed, removed, nil
	}
	for _, t := range (deps.Cache{Root: a.p.DepsCache}).Trees() {
		if total <= limit {
			break
		}
		if err := drop(t.Path, dirSize(t.Path)); err != nil {
			return freed, removed, err
		}
	}
	return freed, removed, nil
}

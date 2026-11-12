package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/cachefs"
	"github.com/yungweng/quorum/internal/deps"
	"github.com/yungweng/quorum/internal/proc"
	"github.com/yungweng/quorum/internal/rundir"
	"github.com/yungweng/quorum/internal/runtmp"
	"github.com/yungweng/quorum/internal/ui"
)

func (a *app) cmdGC(args []string) int {
	parsed, code, done := a.plainArgs("quorum gc [-n | --dry-run]", args, flagSet{"dry-run": noValue, "n": noValue}, 0)
	if done {
		return code
	}
	dry := parsed.boolean("dry-run", "n")
	// Asked for by hand, so it must not answer from a remembered measurement.
	// Forget it and walk the cache as it is now.
	a.forgetCacheSize()
	freed, removed, err := a.collect(dry)
	if err != nil {
		return a.die("cache collection: %v", err)
	}
	// Private TMPDIRs of runs that were killed outright. They live outside the
	// cache, so they count against neither the budget nor the report below.
	if !dry {
		if n := runtmp.Sweep(); n > 0 {
			a.out.Printf("removed %d orphaned temp director%s\n", n, plural(n, "y", "ies"))
		}
	}
	if freed > 0 || removed > 0 {
		verb := "removed"
		if dry {
			verb = "would remove"
		}
		a.out.Printf("%s %d run director%s, %s\n", verb, removed, plural(removed, "y", "ies"), ui.Bytes(freed))
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
		a.rememberCacheSize(a.measureCache())
	}
	return a.cacheBytes
}

func (a *app) setCacheSize(n int64) {
	a.cacheBytes, a.cacheAt = n, time.Now()
}

// rememberedSizeTTL is how long a measurement written by a previous process is
// trusted. cacheSizeTTL only helps within one process, and the agent is a new
// process every poll, so without the file every poll walked the whole cache;
// on a cache of a couple of hundred thousand files that took polls past their
// own interval. The cache only grows when a run of ours finishes, and each of
// those forgets the measurement, so the TTL is a backstop for growth no defer
// observed: a run killed outright, a power loss mid-install, files written in
// by hand. Six hours of that costs at most a run's worth of overshoot before
// the budget is enforced again, which the budget's purpose, keeping days of
// use from filling the disk, does not notice.
const rememberedSizeTTL = 6 * time.Hour

func (a *app) rememberCacheSize(n int64) {
	a.setCacheSize(n)
	line := time.Now().Format(time.RFC3339) + " " + strconv.FormatInt(n, 10) + "\n"
	if err := os.WriteFile(a.p.CacheSizeFile(), []byte(line), 0o644); err != nil {
		a.log.Printf("could not remember the cache size: %v", err)
	}
}

// rememberedCacheSize reads the measurement a previous process left, if there
// is one recent enough to trust. Anything unreadable counts as absent: the
// answer to a broken file is a fresh walk, not an error.
func (a *app) rememberedCacheSize() (int64, bool) {
	data, err := os.ReadFile(a.p.CacheSizeFile())
	if err != nil {
		return 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return 0, false
	}
	at, err := time.Parse(time.RFC3339, fields[0])
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	if age := time.Since(at); age < 0 || age > rememberedSizeTTL {
		return 0, false
	}
	return n, true
}

// forgetCacheSize drops both copies of the measurement, so the next reader
// walks the cache as it is now.
func (a *app) forgetCacheSize() {
	a.cacheBytes, a.cacheAt = 0, time.Time{}
	os.Remove(a.p.CacheSizeFile())
}

func (a *app) measureCache() int64 {
	roots := append(a.p.RunRoots(), a.p.DepsCache, a.trashDir())
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

// runDirs lists every review and babysit run directory, oldest first.
func (a *app) runDirs() []runEntry {
	var runs []runEntry
	for _, root := range a.p.RunRoots() {
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
			runs = append(runs, runEntry{
				path: path, mod: info.ModTime(), live: proc.Claimed(path),
				worktree: worktree, output: output,
			})
		}
	}
	slices.SortFunc(runs, func(a, b runEntry) int { return a.mod.Compare(b.mod) })
	return runs
}

func (a *app) trashDir() string {
	return filepath.Join(rundir.CacheDir(a.p.DepsCache), "trash")
}

type cacheSnapshot struct {
	total int64
	runs  []runEntry
}

// scanCache gets the eviction inventory and its total in one pass over every
// run.
func (a *app) scanCache() cacheSnapshot {
	runs := a.runDirs()
	total := dirSize(a.p.DepsCache) + dirSize(a.trashDir())
	for _, root := range a.p.RunRoots() {
		total += directFileSize(root)
	}
	for _, run := range runs {
		total += run.worktree + run.output
	}
	return cacheSnapshot{total: total, runs: runs}
}

func directFileSize(root string) int64 {
	entries, _ := os.ReadDir(root)
	var total int64
	for _, entry := range entries {
		if info, err := entry.Info(); err == nil && info.Mode().IsRegular() {
			total += info.Size()
		}
	}
	return total
}

// runSizes splits a run directory into its worktree and everything else, in one
// walk. Two calls to dirSize would descend the worktree twice, and the worktree
// is nearly every file a run has.
func runSizes(dir string) (worktree, output int64) {
	prefix := rundir.Worktree(dir) + string(filepath.Separator)
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
	if _, err := os.Stat(rundir.CacheDir(a.p.DepsCache)); err != nil {
		if os.IsNotExist(err) {
			if !dry {
				a.setCacheSize(0)
			}
			return 0, 0, nil
		}
		return 0, 0, err
	}
	// A killed collector may leave entries it already selected in trash. They
	// cannot belong to a live run any more, so finish those removals without
	// taking the startup lock.
	trashSize := dirSize(a.trashDir())
	if trashSize > 0 {
		if dry {
			freed += trashSize
		} else if err := a.cacheRemoveAll(a.trashDir()); err != nil {
			return 0, 0, fmt.Errorf("remove staged cache entries: %w", err)
		} else {
			freed += trashSize
		}
	}
	if limit <= 0 {
		if !dry && freed > 0 {
			a.invalidateRememberedCacheSize(0)
		}
		return freed, 0, nil
	}

	// Measuring a cold cache can take minutes, and the agent gets here every
	// poll. A recent under-budget measurement avoids the walk entirely. An
	// over-budget cache needs a fresh inventory, but that scan stays outside the
	// lock so reviews and babysit runs can start meanwhile.
	if size, ok := a.rememberedCacheSize(); ok {
		if size <= limit && trashSize == 0 {
			if !dry {
				a.setCacheSize(size)
			}
			return freed, 0, nil
		}
	}
	snapshot := a.scanCache()
	if dry {
		// scanCache still sees trash because a dry run did not remove it. Count
		// it once, then plan against what a real collection would retain.
		snapshot.total -= trashSize
	}
	if snapshot.total <= limit {
		if !dry {
			a.rememberCacheSize(snapshot.total)
		}
		return freed, 0, nil
	}

	// Run startup takes the same lock while publishing its claim. Selection is
	// made outside the lock, then each candidate is rechecked and atomically
	// renamed to trash while holding it. Actual recursive deletion happens
	// after unlock, so a large cache never freezes command startup for minutes.
	unlock, err := rundir.LockCache(a.p.DepsCache)
	if err != nil {
		return 0, 0, err
	}
	total := snapshot.total
	batch := ""
	type stagedEntry struct {
		path   string
		source string
		size   int64
		runDir bool
	}
	var staged []stagedEntry
	stage := func(source string, size int64, runDir bool) error {
		if dry {
			if _, err := os.Lstat(source); err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					return nil
				}
				return err
			}
			total -= size
			freed += size
			if runDir {
				removed++
			}
			return nil
		}
		if _, err := os.Lstat(source); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				total -= size
				return nil
			}
			return err
		}
		if batch == "" {
			if err := os.MkdirAll(a.trashDir(), 0o755); err != nil {
				return err
			}
			var err error
			batch, err = os.MkdirTemp(a.trashDir(), "gc-")
			if err != nil {
				return err
			}
		}
		target := filepath.Join(batch, fmt.Sprintf("%04d-%s", len(staged), filepath.Base(source)))
		if err := os.Rename(source, target); err != nil {
			return err
		}
		staged = append(staged, stagedEntry{path: target, source: source, size: size, runDir: runDir})
		total -= size
		return nil
	}

	for _, r := range snapshot.runs {
		if total <= limit {
			break
		}
		if r.live || proc.Claimed(r.path) {
			continue
		}
		if err := stage(rundir.Worktree(r.path), r.worktree, false); err != nil {
			unlock()
			return freed, removed, fmt.Errorf("stage %s for removal: %w", rundir.Worktree(r.path), err)
		}
	}
	// Only the output is still there to free: the round above ends by returning,
	// so reaching this point means it dropped every collectable worktree.
	for _, r := range snapshot.runs {
		if total <= limit {
			break
		}
		if r.live || proc.Claimed(r.path) {
			continue
		}
		if err := stage(r.path, r.output, true); err != nil {
			unlock()
			return freed, removed, fmt.Errorf("stage %s for removal: %w", r.path, err)
		}
	}
	// A run that is still going owns its worktree, and through the symlinks in
	// that worktree it owns shared trees this cannot tell apart from the rest.
	// Rereading the run roots catches a run that started while the collector
	// was walking the cache; startup cannot pass the lock while this check and
	// any dependency staging happen.
	live := slices.ContainsFunc(snapshot.runs, func(r runEntry) bool { return r.live }) ||
		slices.ContainsFunc(a.p.RunRoots(), rundir.AnyClaimed)
	if !live {
		for _, tree := range (deps.Cache{Root: a.p.DepsCache}).Trees() {
			if total <= limit {
				break
			}
			size := dirSize(tree.Path)
			if err := stage(tree.Path, size, false); err != nil {
				unlock()
				return freed, removed, fmt.Errorf("stage %s for removal: %w", tree.Path, err)
			}
		}
	}
	unlock()

	for _, entry := range staged {
		if err := a.cacheRemoveAll(entry.path); err != nil {
			a.invalidateRememberedCacheSize(max(snapshot.total-freed, 0))
			return freed, removed, fmt.Errorf("remove %s: %w", entry.source, err)
		}
		freed += entry.size
		if entry.runDir {
			removed++
		}
	}
	if batch != "" {
		if err := a.cacheRemoveAll(batch); err != nil {
			a.invalidateRememberedCacheSize(max(snapshot.total-freed, 0))
			return freed, removed, fmt.Errorf("remove empty staging directory: %w", err)
		}
	}
	if !dry {
		// Runs could start while the inventory was built. Keep the value for this
		// command's report, but force the next process to measure instead of
		// trusting a total that may not include that concurrent growth.
		a.invalidateRememberedCacheSize(max(snapshot.total-freed, 0))
	}
	return freed, removed, nil
}

func (a *app) cacheRemoveAll(path string) error {
	if a.removeAll != nil {
		return a.removeAll(path)
	}
	return cachefs.RemoveAll(path)
}

func (a *app) invalidateRememberedCacheSize(size int64) {
	a.setCacheSize(size)
	os.Remove(a.p.CacheSizeFile())
}

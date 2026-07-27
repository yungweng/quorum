package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/deps"
	"github.com/yungweng/quorum/internal/proc"
)

// stalePID is above PID_MAX on macOS and above the default pid_max on Linux, so
// it can never name a running process. Its bytes count towards the size of the
// run directory like everything else, which the expectations below spell out.
const stalePID = "2147483647"

// gigabytes expresses a byte count as the GB the config holds, exactly: the
// divisor is a power of two, so nothing is lost on the way back.
func gigabytes(n int64) float64 { return float64(n) / (1024 * 1024 * 1024) }

// fill writes a file of exactly n bytes, creating the directories above it.
func fill(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// makeRun builds a run directory of the given shape, claimed by pid.
func makeRun(t *testing.T, root, name string, worktree, output int, pid string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	fill(t, filepath.Join(dir, "worktree", "checkout"), worktree)
	fill(t, filepath.Join(dir, "output", "final-pr-comment.md"), output)
	if err := os.WriteFile(filepath.Join(dir, proc.ClaimFile), []byte(pid), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// staleRun is what a failed or killed run leaves behind: a claim no process
// answers for any more.
func staleRun(t *testing.T, root, name string, worktree, output int) string {
	t.Helper()
	return makeRun(t, root, name, worktree, output, stalePID)
}

// liveRun is a run directory claimed by this test process, so it reads as still
// in flight.
func liveRun(t *testing.T, root, name string, worktree, output int) string {
	t.Helper()
	return makeRun(t, root, name, worktree, output, strconv.Itoa(os.Getpid()))
}

// depsTree is a published shared dependency tree. Which repository and lock
// hash it belongs to never matters here, only that deps reads it as published:
// an unpublished tree sorts first and would be collected for the wrong reason.
func depsTree(t *testing.T, root string, size int) string {
	t.Helper()
	dir := filepath.Join(root, "owner-repo", "frontend", "578bb61083a3")
	fill(t, filepath.Join(dir, "some-package", "index.js"), size)
	fill(t, filepath.Join(dir, ".complete"), 0)
	trees := (deps.Cache{Root: root}).Trees()
	if len(trees) != 1 || trees[0].Used.IsZero() {
		t.Fatalf("built a tree deps does not read as published: %+v", trees)
	}
	return dir
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func assertCollected(t *testing.T, freed int64, removed int, wantFreed int64, wantRemoved int) {
	t.Helper()
	if freed != wantFreed || removed != wantRemoved {
		t.Fatalf("freed %d bytes in %d run directories, want %d in %d",
			freed, removed, wantFreed, wantRemoved)
	}
}

// A failed review keeps its worktree so --resume-run can pick it up. Below the
// budget there is no reason to take that away.
func TestCollectLeavesFailedRunsAloneBelowBudget(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(1000)
	run := staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 100, 100)

	freed, removed := a.collect(false)
	assertCollected(t, freed, removed, 0, 0)
	if !exists(filepath.Join(run, "worktree")) {
		t.Error("worktree of a failed run was deleted below the budget, --resume-run needs it")
	}
}

// Over the budget the worktree goes first, because a checkout is cheap to
// recreate and the review output beside it is not.
func TestCollectDropsWorktreesBeforeOutput(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(500)
	run := staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100)

	freed, removed := a.collect(false)
	assertCollected(t, freed, removed, 800, 0)
	if exists(filepath.Join(run, "worktree")) {
		t.Error("worktree survived over the budget")
	}
	if !exists(filepath.Join(run, "output", "final-pr-comment.md")) {
		t.Error("review output was taken with the worktree")
	}
}

// The whole point of the claim: a review started from a terminal has no marker
// anywhere, and the collector still must not pull its worktree out from under
// it.
func TestCollectNeverTouchesALiveRun(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(100)
	run := liveRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100)

	freed, removed := a.collect(false)
	assertCollected(t, freed, removed, 0, 0)
	if !exists(filepath.Join(run, "worktree")) {
		t.Error("worktree of a running review was deleted")
	}
}

// Still over the budget with every collectable worktree gone: whole run
// directories go next, oldest first.
func TestCollectDropsWholeRunDirectoriesNext(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(500)
	old := staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 100, 400)
	recent := staleRun(t, a.p.ReviewRuns, "owner-repo-pr-2", 100, 400)
	past := time.Now().Add(-24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}

	// Both worktrees, then everything left in the older directory.
	freed, removed := a.collect(false)
	assertCollected(t, freed, removed, int64(100+100+400+len(stalePID)), 1)
	if exists(old) {
		t.Error("the older run directory survived")
	}
	if !exists(recent) {
		t.Error("the newer run directory was collected while the budget was already met")
	}
}

// Babysit runs live in their own cache, and used to be invisible to gc.
func TestCollectCoversBabysitRuns(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(100)
	run := staleRun(t, a.p.BabysitRuns, "owner-repo-pr-1", 800, 0)

	freed, removed := a.collect(false)
	assertCollected(t, freed, removed, 800, 0)
	if exists(filepath.Join(run, "worktree")) {
		t.Error("babysit worktree survived over the budget")
	}
}

// Dependency trees are the most expensive thing in the cache to rebuild, so
// they go only after everything else, and only when the budget still is not met.
func TestCollectDropsDependencyTreesLast(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(150)
	run := staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 100, 100)
	tree := depsTree(t, a.p.DepsCache, 1000)

	freed, removed := a.collect(false)
	assertCollected(t, freed, removed, int64(100+100+len(stalePID)+1000), 1)
	if exists(run) {
		t.Error("the run directory survived")
	}
	if exists(tree) {
		t.Error("the dependency tree survived while the cache was still over budget")
	}
}

// A running review symlinks straight into the shared trees, and nothing here
// can tell which ones. Being over budget is not worth breaking it for.
func TestCollectSparesDependencyTreesWhileARunIsLive(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(150)
	liveRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 100, 100)
	tree := depsTree(t, a.p.DepsCache, 1000)

	freed, removed := a.collect(false)
	assertCollected(t, freed, removed, 0, 0)
	if !exists(tree) {
		t.Error("a dependency tree a running review may be linked against was deleted")
	}
}

// A dry run reports what a real one would free without touching anything.
func TestCollectDryRunDeletesNothing(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(150)
	run := staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 100, 100)
	tree := depsTree(t, a.p.DepsCache, 1000)

	before := a.cacheSize()
	freed, removed := a.collect(true)
	assertCollected(t, freed, removed, int64(100+100+len(stalePID)+1000), 1)
	if !exists(run) || !exists(tree) {
		t.Error("a dry run deleted something")
	}
	if got := a.cacheSize(); got != before {
		t.Errorf("a dry run moved the remembered cache size from %d to %d", before, got)
	}
}

// No budget means no collecting, which is what CACHE_BUDGET_GB=0 promises.
func TestCollectDoesNothingWithoutABudget(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = 0
	run := staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100)

	freed, removed := a.collect(false)
	assertCollected(t, freed, removed, 0, 0)
	if !exists(filepath.Join(run, "worktree")) {
		t.Error("worktree was deleted with no budget set")
	}
}

// The dashboard redraws every three seconds, so the size it prints is a held
// measurement; collecting must leave that value correct rather than stale.
func TestCollectLeavesTheRememberedSizeCorrect(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(500)
	staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100)

	freed, _ := a.collect(false)
	if got, want := a.cacheSize(), int64(100+len(stalePID)); got != want {
		t.Fatalf("remembered cache size is %d after freeing %d bytes, want %d", got, freed, want)
	}
}

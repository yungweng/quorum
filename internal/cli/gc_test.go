package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/deps"
	"github.com/yungweng/quorum/internal/proc"
	"github.com/yungweng/quorum/internal/runner"
	"github.com/yungweng/quorum/internal/state"
)

// staleClaim is an unlocked claim left by a run that is over. Its bytes count
// towards the size of the run directory like everything else, which the
// expectations below spell out.
const staleClaim = "2147483647"

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

// makeRun builds a run directory of the given shape.
func makeRun(t *testing.T, root, name string, worktree, output int, claim string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	fill(t, filepath.Join(dir, "worktree", "checkout"), worktree)
	fill(t, filepath.Join(dir, "output", "final-pr-comment.md"), output)
	if claim != "" {
		if err := os.WriteFile(filepath.Join(dir, proc.ClaimFile), []byte(claim), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// staleRun is what a failed or killed run leaves behind: a claim no process
// answers for any more.
func staleRun(t *testing.T, root, name string, worktree, output int) string {
	t.Helper()
	return makeRun(t, root, name, worktree, output, staleClaim)
}

// liveRun holds the run's claim lock until the test ends.
func liveRun(t *testing.T, root, name string, worktree, output int) string {
	t.Helper()
	dir := makeRun(t, root, name, worktree, output, "")
	release, err := proc.Claim(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return dir
}

// legacyLiveRun has no run.pid: v1.0.1 exposed liveness through its detached
// review marker and state record instead.
func legacyLiveRun(t *testing.T, a *app, root, name string, worktree, output int) string {
	t.Helper()
	dir := makeRun(t, root, name, worktree, output, "")
	key := "owner/repo#1"
	marker, got, err := runner.Acquire(a.p.RunningDir, key)
	if err != nil || !got {
		t.Fatalf("acquiring legacy marker: got=%v err=%v", got, err)
	}
	t.Cleanup(marker.Release)
	if err := state.Mutate(a.p.StateFile, key, func(rec *state.Record) {
		rec.Status = state.Running
		rec.RunDir = dir
	}); err != nil {
		t.Fatal(err)
	}
	return dir
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

func mustCollect(t *testing.T, a *app, dry bool) (int64, int) {
	t.Helper()
	freed, removed, err := a.collect(dry)
	if err != nil {
		t.Fatal(err)
	}
	return freed, removed
}

// A failed review keeps its worktree so --resume-run can pick it up. Below the
// budget there is no reason to take that away.
func TestCollectLeavesFailedRunsAloneBelowBudget(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(1000)
	run := staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 100, 100)

	freed, removed := mustCollect(t, a, false)
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

	freed, removed := mustCollect(t, a, false)
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

	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, 0, 0)
	if !exists(filepath.Join(run, "worktree")) {
		t.Error("worktree of a running review was deleted")
	}
}

func TestCollectTreatsAReleasedClaimAsCurrent(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(500)
	run := makeRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100, "")
	release, err := proc.Claim(run)
	if err != nil {
		t.Fatal(err)
	}
	release()

	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, 800, 0)
	if exists(filepath.Join(run, "worktree")) {
		t.Error("a completed current-version run received the legacy grace period")
	}
	if !exists(filepath.Join(run, proc.ClaimFile)) {
		t.Error("a released claim lost its current-version sentinel")
	}
}

func TestCollectNeverTouchesARecentLegacyTerminalRunDuringUpgrade(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(100)
	run := makeRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100, "")

	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, 0, 0)
	if !exists(filepath.Join(run, "worktree")) {
		t.Error("worktree of a recent v1.0.1 terminal review was deleted")
	}
}

func TestCollectDropsAnExpiredLegacyTerminalRun(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(500)
	run := makeRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100, "")
	expired := time.Now().Add(-legacyRunGrace - time.Hour)
	if err := os.Chtimes(run, expired, expired); err != nil {
		t.Fatal(err)
	}

	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, 800, 0)
	if exists(filepath.Join(run, "worktree")) {
		t.Error("worktree of an expired v1.0.1 terminal review survived")
	}
}

func TestCollectNeverTouchesALegacyLiveRunDuringUpgrade(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(100)
	run := legacyLiveRun(t, a, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100)
	expired := time.Now().Add(-legacyRunGrace - time.Hour)
	if err := os.Chtimes(run, expired, expired); err != nil {
		t.Fatal(err)
	}

	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, 0, 0)
	if !exists(filepath.Join(run, "worktree")) {
		t.Error("worktree of a running v1.0.1 review was deleted")
	}
}

func TestCollectMatchesLegacyLiveRunsByFullPath(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(500)
	name := "owner-repo-pr-1-20260101-120000"
	live := legacyLiveRun(t, a, a.p.ReviewRuns, name, 100, 100)
	unrelated := makeRun(t, a.p.BabysitRuns, name, 800, 100, "")
	expired := time.Now().Add(-legacyRunGrace - time.Hour)
	for _, run := range []string{live, unrelated} {
		if err := os.Chtimes(run, expired, expired); err != nil {
			t.Fatal(err)
		}
	}

	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, 800, 0)
	if !exists(filepath.Join(live, "worktree")) {
		t.Error("worktree of the live legacy review was deleted")
	}
	if exists(filepath.Join(unrelated, "worktree")) {
		t.Error("an unrelated babysit run with the same basename was protected")
	}
}

func TestCollectProtectsRunsWhenALegacyMarkerCannotBeMapped(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(100)
	run := makeRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100, "")
	expired := time.Now().Add(-legacyRunGrace - time.Hour)
	if err := os.Chtimes(run, expired, expired); err != nil {
		t.Fatal(err)
	}
	marker, got, err := runner.Acquire(a.p.RunningDir, "owner/repo#1")
	if err != nil || !got {
		t.Fatalf("acquiring legacy marker: got=%v err=%v", got, err)
	}
	t.Cleanup(marker.Release)

	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, 0, 0)
	if !exists(filepath.Join(run, "worktree")) {
		t.Error("run was deleted while a v1.0.1 review could not be mapped")
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
	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, int64(100+100+400+len(staleClaim)), 1)
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

	freed, removed := mustCollect(t, a, false)
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

	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, int64(100+100+len(staleClaim)+1000), 1)
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

	freed, removed := mustCollect(t, a, false)
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
	freed, removed := mustCollect(t, a, true)
	assertCollected(t, freed, removed, int64(100+100+len(staleClaim)+1000), 1)
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

	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, 0, 0)
	if !exists(filepath.Join(run, "worktree")) {
		t.Error("worktree was deleted with no budget set")
	}
}

// A fresh installation has no cache root yet. An empty cache needs no lock,
// and collection must not create anything just to prove that.
func TestCollectTreatsAMissingCacheAsEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		dry  bool
	}{
		{name: "real"},
		{name: "dry", dry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := testApp(t)
			cacheRoot := filepath.Join(t.TempDir(), "missing", "quorum")
			a.p.ReviewRuns = filepath.Join(cacheRoot, "reviews")
			a.p.BabysitRuns = filepath.Join(cacheRoot, "babysit")
			a.p.DepsCache = filepath.Join(cacheRoot, "deps")
			a.cfg.CacheBudgetGB = gigabytes(100)
			if tc.dry {
				a.setCacheSize(123)
			}

			freed, removed := mustCollect(t, a, tc.dry)
			assertCollected(t, freed, removed, 0, 0)
			if exists(cacheRoot) {
				t.Error("collection created an empty cache root")
			}
			if tc.dry && a.cacheBytes != 123 {
				t.Errorf("dry collection changed the remembered size to %d", a.cacheBytes)
			}
		})
	}
}

// Collection and its final report share a measurement, so collection must
// leave that value correct rather than stale.
func TestCollectLeavesTheRememberedSizeCorrect(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(500)
	staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100)

	freed, _ := mustCollect(t, a, false)
	if got, want := a.cacheSize(), int64(100+len(staleClaim)); got != want {
		t.Fatalf("remembered cache size is %d after freeing %d bytes, want %d", got, freed, want)
	}
}

func TestCollectReportsRemovalErrors(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(500)
	run := staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100)
	removeErr := errors.New("permission denied")
	a.removeAll = func(string) error { return removeErr }

	freed, removed, err := a.collect(false)
	if !errors.Is(err, removeErr) {
		t.Fatalf("collection error is %v, want %v", err, removeErr)
	}
	assertCollected(t, freed, removed, 0, 0)
	if !exists(filepath.Join(run, "worktree")) {
		t.Error("worktree was removed despite the reported failure")
	}
}

// A cold cache scan can be much slower than starting a run. When no collection
// is needed, the scan must not hold the lock that run startup needs.
func TestCollectDoesNotLockRunStartupBelowBudget(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(1000)
	staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 100, 100)

	unlock, err := proc.LockDir(filepath.Dir(a.p.DepsCache))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, _, err := a.collect(false)
		done <- err
	}()
	<-started

	select {
	case err := <-done:
		unlock()
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		unlock()
		<-done
		t.Fatal("collection waited for the run-startup lock despite being below budget")
	}
}

// Run startup and collection share the dependency-root lock. A startup that
// gets there first can publish its claim before collection checks liveness, so
// the collector cannot delete a tree the new run is about to link.
func TestCollectSerializesDependencyEvictionWithRunStartup(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(150)
	tree := depsTree(t, a.p.DepsCache, 1000)

	unlock, err := proc.LockDir(filepath.Dir(a.p.DepsCache))
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		_, _, err := a.collect(false)
		done <- err
	}()
	<-started
	select {
	case err := <-done:
		unlock()
		if err != nil {
			t.Fatal(err)
		}
		t.Fatal("collection did not wait for the run-startup lock")
	case <-time.After(50 * time.Millisecond):
	}

	liveRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 100, 100)
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !exists(tree) {
		t.Error("dependency tree was deleted after a run claimed its worktree")
	}
}

// rememberSize plants the measurement file an earlier process would have left.
func rememberSize(t *testing.T, a *app, at time.Time, n int64) {
	t.Helper()
	line := at.Format(time.RFC3339) + " " + strconv.FormatInt(n, 10) + "\n"
	if err := os.WriteFile(a.rememberedSizePath(), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The agent measures the cache on every poll, from a fresh process each time.
// A recent measurement that was under budget is trusted as it stands, walk and
// eviction included: that staleness is the price of polls that finish inside
// their own interval.
func TestCollectTrustsARecentMeasurementUnderBudget(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(500)
	run := staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100)
	rememberSize(t, a, time.Now(), 400)

	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, 0, 0)
	if !exists(filepath.Join(run, "worktree")) {
		t.Error("collection walked and deleted despite a recent under-budget measurement")
	}
}

// Once the measurement is too old it means nothing, and the walk is back.
func TestCollectIgnoresAStaleMeasurement(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(500)
	run := staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100)
	rememberSize(t, a, time.Now().Add(-rememberedSizeTTL-time.Minute), 400)

	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, 800, 0)
	if exists(filepath.Join(run, "worktree")) {
		t.Error("worktree survived although the remembered measurement had expired")
	}
}

// A measurement file that does not parse is treated as absent, not as an error.
func TestCollectIgnoresAGarbageMeasurement(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(500)
	run := staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100)
	if err := os.WriteFile(a.rememberedSizePath(), []byte("not a measurement\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, 800, 0)
	if exists(filepath.Join(run, "worktree")) {
		t.Error("worktree survived although the measurement file was garbage")
	}
}

// What one process measures, the next process reads. This is the whole point:
// the agent's polls are each a fresh process, and only the file keeps them
// from walking the cache every time.
func TestCollectRemembersItsMeasurementForTheNextProcess(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(1000)
	staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 100, 100)

	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, 0, 0)

	next := &app{cfg: a.cfg, p: a.p, log: a.log}
	size, ok := next.rememberedCacheSize()
	if !ok {
		t.Fatal("collection left no measurement for the next process")
	}
	if want := int64(100 + 100 + len(staleClaim)); size != want {
		t.Fatalf("remembered measurement is %d, want %d", size, want)
	}
}

// After an eviction the file holds the new total, so the next poll neither
// walks nor evicts again.
func TestCollectRemembersTheTotalAfterEviction(t *testing.T) {
	a := testApp(t)
	a.cfg.CacheBudgetGB = gigabytes(500)
	staleRun(t, a.p.ReviewRuns, "owner-repo-pr-1", 800, 100)

	freed, removed := mustCollect(t, a, false)
	assertCollected(t, freed, removed, 800, 0)

	next := &app{cfg: a.cfg, p: a.p, log: a.log}
	size, ok := next.rememberedCacheSize()
	if !ok {
		t.Fatal("eviction left no measurement for the next process")
	}
	if want := int64(100 + len(staleClaim)); size != want {
		t.Fatalf("remembered measurement is %d after eviction, want %d", size, want)
	}
}

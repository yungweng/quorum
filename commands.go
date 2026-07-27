package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/deps"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/proc"
	"github.com/yungweng/quorum/internal/runner"
	"github.com/yungweng/quorum/internal/ui"
)

// cmdRun reviews one pull request now, whatever the queue and the filters say.
func (a *app) cmdRun(args []string) int {
	if len(args) == 0 {
		return a.die("usage: quorum run <pr-url> | <owner/repo#number> | <number>")
	}
	repo, number, err := a.resolvePR(args[0])
	if err != nil {
		return a.die("%v", err)
	}
	t, err := a.findTools()
	if err != nil {
		return a.die("%v", err)
	}
	if err := a.p.EnsureDirs(); err != nil {
		return a.die("%v", err)
	}

	key := fmt.Sprintf("%s#%d", repo, number)
	marker, got, err := runner.Acquire(a.p.RunningDir, key)
	if err != nil {
		return a.die("%v", err)
	}
	if !got {
		return a.die("a review of %s is already running", key)
	}
	defer marker.Release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := a.newGH(t.GH)
	details, err := client.PRDetails(ctx, repo, number)
	if err != nil {
		return a.die("%v", err)
	}
	a.log.Echo = func(s string) { fmt.Println(s) }
	r := &runner.Runner{Cfg: a.cfg, P: a.p, Log: a.log, GitBin: t.Git, GHBin: t.GH,
		CodexBin: t.Codex, DirenvBin: t.Direnv, GH: client, Git: a.newGit(t.Git)}
	if err := r.Review(ctx, key, repo, number, details.HeadRefOid, details.Title, ""); err != nil {
		return 1
	}
	return 0
}

var prURL = regexp.MustCompile(`^https://github\.com/([^/]+/[^/]+)/pull/(\d+)`)

// resolvePR accepts a pull request URL, owner/repo#number, or a bare number
// when the working directory is inside a repository.
func (a *app) resolvePR(arg string) (string, int, error) {
	if m := prURL.FindStringSubmatch(arg); m != nil {
		n, _ := strconv.Atoi(m[2])
		return m[1], n, nil
	}
	if repo, num, ok := strings.Cut(arg, "#"); ok && strings.Contains(repo, "/") {
		n, err := strconv.Atoi(num)
		if err != nil {
			return "", 0, fmt.Errorf("%q does not end in a PR number", arg)
		}
		return repo, n, nil
	}
	if n, err := strconv.Atoi(arg); err == nil {
		gh, err := exec.LookPath("gh")
		if err != nil {
			return "", 0, fmt.Errorf("gh not found")
		}
		out, err := exec.Command(gh, "repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner").Output()
		if err != nil {
			return "", 0, fmt.Errorf("not inside a repository, use owner/repo#%d", n)
		}
		return strings.TrimSpace(string(out)), n, nil
	}
	return "", 0, fmt.Errorf("cannot read %q as a pull request", arg)
}

// cmdLogs follows the log file, printing the tail first.
func (a *app) cmdLogs(args []string) int {
	n := 50
	follow := true
	for _, arg := range args {
		switch arg {
		case "-n", "--no-follow":
			follow = false
		default:
			if v, err := strconv.Atoi(arg); err == nil {
				n = v
			}
		}
	}
	for _, line := range a.log.Tail(n) {
		fmt.Println(line)
	}
	if !follow {
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	f, err := os.Open(a.p.Log)
	if err != nil {
		if os.IsNotExist(err) {
			<-ctx.Done()
			return 0
		}
		return a.die("%v", err)
	}
	defer f.Close()
	f.Seek(0, io.SeekEnd)

	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return 0
		default:
		}
		n, err := f.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
			continue
		}
		if err != nil && err != io.EOF {
			return a.die("%v", err)
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// cmdWatch redraws the dashboard until interrupted.
func (a *app) cmdWatch(args []string) int {
	_ = args
	if !a.out.Color {
		// Without a terminal there is nothing to redraw into.
		return a.cmdStatus(nil)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ends := newEndStates()
	if t, err := a.findTools(); err == nil {
		go a.trackEnds(ctx, t.GH, ends)
	}

	a.out.AltScreen(true)
	defer a.out.AltScreen(false)

	var frame bytes.Buffer
	painted := ""
	for {
		// Config and terminal size can change while watching.
		a.reload()
		// The frame is built in memory and reaches the terminal in one write.
		// Drawing straight to the screen shows it half finished on every pass.
		frame.Reset()
		screen := a.out.To(&frame)
		shown := a.dashboard(screen, ends.snapshot())
		screen.Printf("%s\n", screen.Dim("ctrl-c to leave"))
		ends.want(shown)

		// Most passes change nothing at all. Painting only what differs keeps
		// the terminal quiet instead of rewriting an identical screen.
		if next := frame.String(); next != painted {
			a.out.Paint(next)
			painted = next
		}

		select {
		case <-ctx.Done():
			return 0
		case <-time.After(3 * time.Second):
		}
	}
}

// endStates caches how the pull requests on screen ended, so the redraw never
// has to wait for GitHub.
type endStates struct {
	mu     sync.Mutex
	state  map[string]string
	keys   []string
	change chan struct{}
}

func newEndStates() *endStates {
	return &endStates{state: map[string]string{}, change: make(chan struct{}, 1)}
}

// snapshot is what the dashboard reads: a copy, so rendering never holds the
// lock the refresher needs.
func (e *endStates) snapshot() map[string]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return maps.Clone(e.state)
}

// want records which pull requests are currently visible. A changed set wakes
// the refresher, so a pull request that appears is looked up straight away
// instead of waiting out the interval.
func (e *endStates) want(keys []string) {
	e.mu.Lock()
	same := slices.Equal(e.keys, keys)
	e.keys = keys
	e.mu.Unlock()
	if same {
		return
	}
	select {
	case e.change <- struct{}{}:
	default: // a wake-up is already pending, one is enough
	}
}

func (e *endStates) wanted() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.keys)
}

func (e *endStates) store(states map[string]string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.state = states
}

// trackEnds keeps the merge state of the visible pull requests roughly current.
//
// It runs beside the redraw and never inside it. The dashboard repaints every
// few seconds, and asking GitHub at that rate would make the screen wait on the
// network and earn a rate limit for a fact that only changes when somebody
// presses a button. One batched query covers every visible pull request.
func (a *app) trackEnds(ctx context.Context, ghBin string, e *endStates) {
	client := gh.New(ghBin)
	// No retries and a short deadline: this is decoration, and the next pass is
	// thirty seconds away, which is sooner than a retried call would answer.
	// Logging is off as well, because the logger echoes to the screen this is
	// drawing on.
	client.Attempts = 1
	client.Timeout = 20 * time.Second
	client.Log = nil

	for {
		select {
		case <-ctx.Done():
			return
		case <-e.change:
		case <-time.After(30 * time.Second):
		}
		keys := e.wanted()
		if len(keys) == 0 {
			continue
		}
		states, err := client.PRStates(ctx, keys)
		if err != nil {
			// Nothing on the screen depends on this, so a failed lookup keeps
			// the previous answer rather than blanking what it already knows.
			continue
		}
		e.store(states)
	}
}

// reload picks up config edits and a resized terminal between redraws.
func (a *app) reload() {
	cfg, err := config.Load(a.p.Config)
	a.cfg, a.configErr = cfg, err
	a.out = ui.New(os.Stdout)
}

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
		a.out.Printf("%s %d run director%s, %s\n", verb, removed, plural(removed), ui.Bytes(freed))
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

// budgetBytes is the cache budget in bytes, or 0 when none is set.
func (a *app) budgetBytes() int64 {
	return int64(a.cfg.CacheBudgetGB * 1024 * 1024 * 1024)
}

// cacheSizeTTL is how long a measurement is reused. `quorum watch` redraws
// every three seconds and the dependency trees run to tens of thousands of
// files, so measuring per frame would spend the whole interval in stat calls
// to render a line that reads the same either way.
const cacheSizeTTL = time.Minute

// cacheSize is everything the budget covers: both run caches and the shared
// dependency trees. The managed clones are not in it, because their size is
// bounded by how many repositories you review rather than by how often.
func (a *app) cacheSize() int64 {
	if time.Since(a.cacheAt) > cacheSizeTTL {
		a.setCacheSize(dirSize(a.p.ReviewRuns) + dirSize(a.p.BabysitRuns) + dirSize(a.p.DepsCache))
	}
	return a.cacheBytes
}

func (a *app) setCacheSize(n int64) {
	a.cacheBytes, a.cacheAt = n, time.Now()
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
			runs = append(runs, runEntry{
				path: path, mod: info.ModTime(), live: proc.Claimed(path),
				worktree: worktree, output: output,
			})
		}
	}
	slices.SortFunc(runs, func(a, b runEntry) int { return a.mod.Compare(b.mod) })
	return runs
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
	a.setCacheSize(dirSize(a.p.ReviewRuns) + dirSize(a.p.BabysitRuns) + dirSize(a.p.DepsCache))
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
	drop := func(path string, size int64) bool {
		if !dry {
			if err := os.RemoveAll(path); err != nil {
				return false
			}
		}
		total -= size
		freed += size
		return true
	}

	for _, r := range runs {
		if total <= limit {
			return freed, removed, nil
		}
		if r.live {
			continue
		}
		drop(filepath.Join(r.path, "worktree"), r.worktree)
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
		if drop(r.path, r.output) {
			removed++
		}
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
		drop(t.Path, dirSize(t.Path))
	}
	return freed, removed, nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// small helpers shared by the commands.

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func ownerOf(repo string) string {
	owner, _, _ := strings.Cut(repo, "/")
	return owner
}

func itoa(n int) string { return strconv.Itoa(n) }

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// secs turns a configured interval into a duration.
func secs(n int) time.Duration { return time.Duration(n) * time.Second }

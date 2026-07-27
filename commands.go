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
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/runner"
	"github.com/yungweng/quorum/internal/state"
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

// cmdGC trims the review cache to its budget and clears anything left behind by
// a run that did not finish.
func (a *app) cmdGC(args []string) int {
	dry := false
	for _, arg := range args {
		if arg == "--dry-run" || arg == "-n" {
			dry = true
		}
	}
	freed, removed := a.collect(dry)
	verb := "removed"
	if dry {
		verb = "would remove"
	}
	a.out.Printf("%s %d run director%s, %s\n", verb, removed, plural(removed), ui.Bytes(freed))
	return 0
}

// runEntry is one run directory of pr-codex-review with its size and age.
type runEntry struct {
	path string
	mod  time.Time
	live bool
}

// collect deletes the worktrees of finished runs first, since they hold nearly
// all of the space and are reproducible, and only then whole run directories,
// oldest first, until the cache fits its budget.
func (a *app) collect(dry bool) (freed int64, removed int) {
	entries, err := os.ReadDir(a.p.ReviewRuns)
	if err != nil {
		return 0, 0
	}
	// Run directories in use by a live review must not be touched.
	inUse := map[string]bool{}
	file, _ := state.Read(a.p.StateFile)
	for _, m := range runner.Live(a.p.RunningDir) {
		if rec, ok := file.PRs[m.Key]; ok && rec.RunDir != "" {
			inUse[filepath.Base(rec.RunDir)] = true
		}
	}

	var runs []runEntry
	for _, e := range entries {
		// deps holds the shared dependency trees, which pr-codex-review keys by
		// lock file hash and manages itself.
		if !e.IsDir() || e.Name() == "deps" {
			continue
		}
		path := filepath.Join(a.p.ReviewRuns, e.Name())
		info, err := e.Info()
		if err != nil {
			continue
		}
		runs = append(runs, runEntry{path: path, mod: info.ModTime(), live: inUse[e.Name()]})
	}

	// Worktrees of runs that are over: the review output stays readable.
	for _, r := range runs {
		if r.live {
			continue
		}
		wt := filepath.Join(r.path, "worktree")
		size := dirSize(wt)
		if size == 0 {
			continue
		}
		if !dry {
			if err := os.RemoveAll(wt); err != nil {
				continue
			}
		}
		freed += size
	}

	if a.cfg.CacheBudgetGB <= 0 {
		return freed, removed
	}
	limit := int64(a.cfg.CacheBudgetGB * 1024 * 1024 * 1024)
	total := dirSize(a.p.ReviewRuns)
	if dry {
		total -= freed
	}
	if total <= limit {
		return freed, removed
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].mod.Before(runs[j].mod) })
	for _, r := range runs {
		if total <= limit {
			break
		}
		if r.live {
			continue
		}
		size := dirSize(r.path)
		if !dry {
			if err := os.RemoveAll(r.path); err != nil {
				continue
			}
		}
		total -= size
		freed += size
		removed++
	}
	return freed, removed
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

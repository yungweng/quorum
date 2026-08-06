package cli

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/history"
	"github.com/yungweng/quorum/internal/runner"
	"github.com/yungweng/quorum/internal/state"
	"github.com/yungweng/quorum/internal/ui"
)

// candidate is one pull request the search returned, paired with what quorum
// already knows about it.
type candidate struct {
	pr    gh.PR
	key   string
	rec   state.Record
	reqAt string // the review request that would trigger a review
}

func (a *app) cmdPoll(args []string) int {
	_ = args
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	t, err := a.findTools()
	if err != nil {
		a.log.Printf("%v", err)
		return a.die("%v", err)
	}
	if err := a.p.EnsureDirs(); err != nil {
		return a.die("%v", err)
	}
	if a.configErr != nil {
		a.log.Printf("config problem, using defaults where unclear: %v", a.configErr)
	}

	// Only the poll itself is serialised. Reviews run detached and are tracked
	// by their markers, so a long review never blocks the next poll.
	unlock, held, err := tryLock(filepath.Join(a.p.StateDir, "poll.lock"))
	if err != nil {
		return a.die("%v", err)
	}
	if !held {
		a.log.Printf("another poll is still running, skipped")
		return 0
	}
	defer unlock()

	// An upgrade swaps the binary behind the symlink, so the next poll already
	// runs the new version; the plist keeps its old interval, PATH and template
	// until somebody reinstalls. findTools widened PATH above, so the
	// comparison sees the same PATH install would bake in.
	if runtime.GOOS == "darwin" && a.plistStale() {
		a.log.Printf("the installed launchd job is out of date, refresh it with: quorum install")
	}

	// The sweeps inside a run only drop what is a week old, which a couple of
	// days of heavy use fills a disk well ahead of. Enforcing the budget here is
	// what keeps that from needing somebody to notice and run `quorum gc`.
	freed, _, err := a.collect(false)
	if err != nil {
		a.log.Printf("cache collection failed: %v", err)
		return 1
	}
	if freed > 0 {
		a.log.Printf("cache was over its budget, freed %s", ui.Bytes(freed))
	}

	// A review whose process was killed leaves a record claiming to be running.
	// Turning those into failures here is what lets them be picked up again.
	if n := a.clearOrphans(); n > 0 {
		a.log.Printf("%d review(s) had stopped without finishing, queued for another attempt", n)
	}

	client := a.newGH(t.GH)
	login, err := client.Login(ctx)
	if err != nil {
		a.log.Printf("cannot reach GitHub: %v", err)
		return 1
	}

	var teams []string
	if a.cfg.IncludeTeams {
		teams = a.cfg.Teams
		if len(teams) == 0 {
			teams, err = client.Teams(ctx)
			if err != nil {
				// Team discovery failing must not stop the direct requests.
				a.log.Printf("could not list your teams: %v", err)
			}
		}
	}

	prs, err := a.search(ctx, client, teams, login)
	if err != nil {
		a.log.Printf("search failed: %v", err)
		return 1
	}
	a.touchHeartbeat(len(prs))

	if len(prs) == 0 {
		a.log.Printf("no open review requests")
		a.forgetStalePending(nil)
		return 0
	}

	// Work out what each pull request needs before starting anything, so the
	// queue is complete in the state file even when no slot is free.
	var ready []candidate
	for _, pr := range prs {
		c, start := a.classify(ctx, client, pr, login, teams)
		if start {
			ready = append(ready, c)
		}
	}
	a.forgetStalePending(prs)

	// Oldest request first, so nothing starves behind a busy repository.
	sort.Slice(ready, func(i, j int) bool { return ready[i].reqAt < ready[j].reqAt })

	a.log.Printf("%d open review request(s), %d to review", len(prs), len(ready))
	if len(ready) == 0 {
		return 0
	}

	// An exhausted usage limit dooms every new run until the provider resets
	// it. The records stay untouched apart from the visible queue status, so
	// the first poll after the pause picks the same requests up again.
	if until, paused := runner.ReadPause(a.p.StateDir); paused {
		a.log.Printf("usage limit: paused until %s, not starting new reviews", until.Format(time.RFC3339))
		a.markQueued(ready, state.Deferred, "usage limit, retrying after "+until.Format(time.RFC3339))
		return 0
	}

	slots := a.cfg.MaxConcurrent - len(runner.Live(a.p.RunningDir))
	if slots <= 0 {
		a.log.Printf("at capacity: %d of %d review slot(s) in use, %d waiting",
			a.cfg.MaxConcurrent-slots, a.cfg.MaxConcurrent, len(ready))
		a.markQueued(ready, state.Pending, "waiting for a free slot")
		return 0
	}
	if load, ok := runner.LoadAvg1(); ok && a.cfg.LoadLimit > 0 && load > a.cfg.LoadLimit {
		a.log.Printf("holding back: load %.1f is above the limit of %.1f", load, a.cfg.LoadLimit)
		a.markQueued(ready, state.Deferred, "system busy")
		return 0
	}

	started := 0
	for _, c := range ready {
		if started >= slots {
			a.markQueued(ready[started:], state.Pending, "waiting for a free slot")
			break
		}
		if a.start(t, c) {
			started++
		}
	}
	return 0
}

// search collects the direct review requests and, separately, the ones aimed at
// a team: GitHub's review-requested: qualifier does not match those despite
// what its documentation suggests.
func (a *app) search(ctx context.Context, client *gh.Client, teams []string, login string) ([]gh.PR, error) {
	var scope []string
	for _, o := range a.cfg.Orgs {
		scope = append(scope, "--owner="+o)
	}
	for _, r := range a.cfg.Repos {
		scope = append(scope, "--repo="+r)
	}

	prs, err := client.SearchReviewRequested(ctx, scope)
	if err != nil {
		return nil, err
	}
	for _, team := range teams {
		more, err := client.SearchTeamReviewRequested(ctx, team, scope)
		if err != nil {
			// One unreachable team must not hide everything else.
			a.log.Printf("team search for %s failed: %v", team, err)
			continue
		}
		prs = append(prs, more...)
	}

	seen := map[string]bool{}
	var out []gh.PR
	for _, pr := range prs {
		if seen[pr.Key()] {
			continue
		}
		seen[pr.Key()] = true
		if a.cfg.SkipOwn && pr.Author.Login == login {
			continue
		}
		out = append(out, pr)
	}
	return out, nil
}

// classify decides what to do with one pull request and records it. The second
// return value reports whether it should be reviewed now.
func (a *app) classify(ctx context.Context, client *gh.Client, pr gh.PR, login string, teams []string) (candidate, bool) {
	key := pr.Key()
	repo := pr.Repository.NameWithOwner
	file, _ := state.Read(a.p.StateFile)
	c := candidate{pr: pr, key: key, rec: file.PRs[key]}
	if c.rec.Title != pr.Title || c.rec.Author != pr.Author.Login {
		a.record(key, func(rec *state.Record) {
			rec.Title = pr.Title
			rec.Author = pr.Author.Login
		})
		c.rec.Title = pr.Title
		c.rec.Author = pr.Author.Login
	}

	skip := func(reason string) (candidate, bool) {
		if c.rec.Status != state.Skipped || c.rec.Reason != reason {
			a.log.Printf("%s: skipped, %s", key, reason)
		}
		a.record(key, func(rec *state.Record) {
			rec.Title = pr.Title
			rec.Mark(state.Skipped, reason)
		})
		return c, false
	}

	if slices.Contains(a.cfg.ExcludeRepos, repo) {
		return skip("excluded by config")
	}
	if a.cfg.SkipDrafts && pr.IsDraft {
		return skip("draft")
	}
	if a.cfg.SkipBots && pr.Author.IsBot {
		return skip("opened by a bot")
	}

	// The expensive part. The timeline only changes when the pull request does,
	// so an unchanged updatedAt means the answer from last time still holds.
	reqAt := c.rec.SeenReqAt
	if c.rec.TimelineAt != pr.UpdatedAt {
		var err error
		reqAt, err = client.LatestReviewRequest(ctx, repo, pr.Number, login, gh.TeamSlugsFor(ownerOf(repo), teams))
		if err != nil {
			// A failed lookup is not a decision. Say so plainly and try again
			// on the next poll instead of recording a skip that never happened.
			a.log.Printf("%s: could not read the timeline, will retry: %v", key, err)
			return c, false
		}
		a.record(key, func(rec *state.Record) {
			rec.Title = pr.Title
			rec.TimelineAt = pr.UpdatedAt
			rec.SeenReqAt = reqAt
		})
	}
	if reqAt == "" {
		return skip("no review request for you on this PR")
	}
	c.reqAt = reqAt

	if c.rec.ReqAt == reqAt {
		// Already reviewed for this request. Leave the finished record alone.
		return c, false
	}
	if int(c.rec.Fails) >= a.cfg.MaxRetries {
		// A give-up binds to the request it gave up on: giving up recorded
		// that ReqAt below, and the equality check above keeps it quiet. This
		// request is a newer one, so it gets a fresh retry budget instead of
		// inheriting a failure count that may describe a long-gone problem,
		// such as an exhausted usage limit.
		if c.rec.Status == state.GaveUp {
			a.log.Printf("%s: new review request after giving up, retrying with a fresh budget", key)
			a.record(key, func(rec *state.Record) { rec.Fails = 0 })
			c.rec.Fails = 0
			return c, true
		}
		a.log.Printf("%s: giving up after %d failed attempts, retry with: quorum run %s", key, c.rec.Fails, key)
		a.record(key, func(rec *state.Record) {
			rec.Mark(state.GaveUp, "too many failed attempts")
			rec.ReqAt = reqAt
		})
		return c, false
	}
	return c, true
}

// start launches one review as a detached process that outlives this poll.
func (a *app) start(t tools, c candidate) bool {
	repo := c.pr.Repository.NameWithOwner
	marker, got, err := runner.Acquire(a.p.RunningDir, c.key)
	if err != nil {
		a.log.Printf("%s: could not claim the review: %v", c.key, err)
		return false
	}
	if !got {
		return false
	}

	// The head SHA and the fork flag are the only things the search does not
	// carry, so they are fetched last, once everything else says go.
	client := a.newGH(t.GH)
	details, err := client.PRDetails(context.Background(), repo, c.pr.Number)
	if err != nil {
		marker.Release()
		a.log.Printf("%s: could not read the PR, will retry: %v", c.key, err)
		return false
	}
	if details.State != "OPEN" {
		marker.Release()
		a.record(c.key, func(rec *state.Record) {
			rec.Mark(state.Skipped, "no longer open")
		})
		return false
	}
	// Reviewing runs the PR's own code locally through direnv and codex, so a
	// fork stays out unless the user opted in.
	if a.cfg.SkipForks && details.IsCrossRepository {
		marker.Release()
		a.log.Printf("%s: skipped, fork PR (set SKIP_FORKS=0 to allow)", c.key)
		a.record(c.key, func(rec *state.Record) {
			rec.Mark(state.Skipped, "fork PR, set SKIP_FORKS=0 to allow")
		})
		return false
	}

	self, err := os.Executable()
	if err != nil {
		marker.Release()
		return false
	}
	cmd := exec.Command(self, "_review", c.key, repo, itoa(c.pr.Number),
		details.HeadRefOid, c.pr.Title, c.pr.Author.Login, c.reqAt, marker.Path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	devnull, _ := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if devnull != nil {
		defer devnull.Close()
		cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	}
	if err := cmd.Start(); err != nil {
		marker.Release()
		a.log.Printf("%s: could not start the review: %v", c.key, err)
		return false
	}
	// The marker now has to name the process that actually does the work. If
	// that fails it still names this poll, which is about to exit, and the next
	// poll would see a dead holder and start the same review again.
	if err := marker.Adopt(cmd.Process.Pid); err != nil {
		a.log.Printf("%s: could not record the review process: %v", c.key, err)
	}
	_ = cmd.Process.Release()

	a.log.Printf("%s: review started (pid %d, %d reviewers)", c.key, cmd.Process.Pid, a.cfg.Reviewers)
	a.record(c.key, func(rec *state.Record) {
		rec.Title = c.pr.Title
		rec.Author = c.pr.Author.Login
		rec.Mark(state.Running, "")
		rec.SHA = details.HeadRefOid
		rec.StartedAt = state.Now()
	})
	return true
}

// cmdReviewOne is the detached child that performs one review. It owns the
// marker for as long as it runs.
func (a *app) cmdReviewOne(args []string) int {
	if len(args) < 8 {
		return a.die("_review is internal and takes key repo number sha title author reqAt marker")
	}
	key, repo, number, sha, title, author, reqAt, markerPath :=
		args[0], args[1], args[2], args[3], args[4], args[5], args[6], args[7]

	marker := &runner.Marker{Key: key, Path: markerPath, PID: os.Getpid()}
	defer marker.Release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	t, err := a.findTools()
	if err != nil {
		return a.die("%v", err)
	}
	if err := a.p.EnsureDirs(); err != nil {
		return a.die("%v", err)
	}
	r := &runner.Runner{
		Cfg: a.cfg, P: a.p, Log: a.log,
		GitBin: t.Git, GHBin: t.GH, CodexBin: t.Codex, ClaudeBin: t.Claude, DirenvBin: t.Direnv,
		GH: a.newGH(t.GH), Git: a.newGit(t.Git),
	}
	if err := r.Review(ctx, key, repo, atoi(number), sha, title, author, reqAt, runner.InvocationAgent); err != nil {
		return 1
	}
	return 0
}

// markQueued records pull requests that are waiting, so the queue is visible
// without reading the log.
func (a *app) markQueued(cs []candidate, status, reason string) {
	for _, c := range cs {
		a.record(c.key, func(rec *state.Record) {
			rec.Title = c.pr.Title
			rec.Author = c.pr.Author.Login
			rec.Mark(status, reason)
		})
	}
}

// forgetStalePending clears queued records for pull requests the search no
// longer returns, so a withdrawn request does not sit in the queue forever.
func (a *app) forgetStalePending(prs []gh.PR) {
	open := map[string]bool{}
	for _, pr := range prs {
		open[pr.Key()] = true
	}
	file, err := state.Read(a.p.StateFile)
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, m := range runner.Live(a.p.RunningDir) {
		live[m.Key] = true
	}
	for key, rec := range file.PRs {
		queued := rec.Status == state.Pending || rec.Status == state.Deferred || rec.Status == state.Running
		if !queued || open[key] || live[key] {
			continue
		}
		a.record(key, func(r *state.Record) {
			r.Mark(state.Skipped, "review request withdrawn or already handled")
		})
	}
}

// record updates the state file, and adds the change to the history log when
// it is one the log wants. Every decision the poll reaches goes through here,
// which is what keeps the two stores from drifting apart.
func (a *app) record(key string, fn func(*state.Record)) {
	var before, after state.Record
	capture := func(rec *state.Record) {
		before = *rec
		fn(rec)
		after = *rec
	}
	if err := state.Mutate(a.p.StateFile, key, capture); err != nil {
		a.log.Printf("could not update state for %s: %v", key, err)
		return
	}
	if run, ok := history.FromState(key, before, after, history.SourceAgent); ok {
		if err := history.Append(a.p.HistoryFile, run); err != nil {
			a.log.Printf("could not add %s to the history log: %v", key, err)
		}
	}
}

// touchHeartbeat records that a poll completed, which is what status uses to
// tell "the agent is loaded" from "the agent is actually working".
func (a *app) touchHeartbeat(open int) {
	path := filepath.Join(a.p.StateDir, "last-poll")
	os.WriteFile(path, []byte(time.Now().Format(time.RFC3339)+" "+itoa(open)+"\n"), 0o644)
}

// clearOrphans turns interrupted reviews into failures so the next poll can
// pick them up again instead of leaving them stuck as running.
//
// It lives here rather than in doctor.go because every poll starts with it:
// it is recovery the agent does for itself, not a diagnostic somebody asks for.
func (a *app) clearOrphans() int {
	file, err := state.Read(a.p.StateFile)
	if err != nil {
		return 0
	}
	live := map[string]bool{}
	for _, m := range runner.Live(a.p.RunningDir) {
		live[m.Key] = true
	}
	n := 0
	for key, rec := range file.PRs {
		if rec.Status != state.Running || live[key] {
			continue
		}
		a.record(key, func(r *state.Record) {
			r.Mark(state.Failed, "the review process stopped before it finished")
			r.Fails++
		})
		n++
	}
	return n
}

// ownerOf is the owner half of "owner/repo".
func ownerOf(repo string) string {
	owner, _, _ := strings.Cut(repo, "/")
	return owner
}

func itoa(n int) string { return strconv.Itoa(n) }

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// tryLock takes the poll lock without waiting. held is false when another poll
// already has it.
func tryLock(path string) (release func(), held bool, err error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, true, nil
}

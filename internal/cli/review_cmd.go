package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/yungweng/quorum/internal/history"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/ui"
)

var reviewBoolFlags = map[string]bool{
	"dry-run": true, "keep-worktree": true, "no-direnv": true,
	"allow-envrc-change": true, "no-notify": true, "h": true, "help": true,
	// Retired no-ops, accepted so old invocations do not break. They describe
	// the default behaviour now.
	"post": true, "cleanup": true, "allow-base-drift": true,
}

var reviewFlags = map[string]bool{
	"n": true, "runs": true, "concurrency": true, "model": true, "effort": true,
	"base": true, "review-timeout": true, "min-successful": true, "resume-run": true,
}

func (a *app) cmdReview(argv []string) int {
	allowed := map[string]bool{}
	for k := range reviewBoolFlags {
		allowed[k] = true
	}
	for k := range reviewFlags {
		allowed[k] = true
	}

	args, err := parseArgs(argv, reviewBoolFlags)
	if err != nil {
		return a.die("%v", err)
	}
	if args.boolean("h", "help") {
		a.reviewUsage()
		return exitOK
	}
	if bad := args.unknown(allowed); len(bad) > 0 {
		return a.die("unknown option: --%s", bad[0])
	}
	if len(args.pos) > 1 {
		a.reviewUsage()
		return exitError
	}

	t, err := a.findTools()
	if err != nil {
		return a.die("%v", err)
	}
	repoRoot, repo, err := a.hereRepo(t)
	if err != nil {
		return a.die("%v", err)
	}
	number, err := resolveReviewNumber(args.pos, repo)
	if err != nil {
		return a.die("%v", err)
	}

	o := review.Options{
		Repo: repo, Number: number, RepoRoot: repoRoot,
		Model:          a.cfg.ReviewModel,
		Effort:         a.cfg.ReviewEffort,
		Runs:           a.cfg.Reviewers,
		Post:           a.cfg.Post,
		UseDirenv:      t.Direnv != "",
		RunsDir:        a.p.ReviewRuns,
		SharedRunsDirs: []string{a.p.BabysitRuns},
		DepsDir:        a.p.DepsCache,
		CodexBin:       t.Codex,
		DirenvBin:      t.Direnv,
	}
	if o.Runs, err = args.intVal(o.Runs, "n", "runs"); err != nil {
		return a.die("%v", err)
	}
	if o.Concurrency, err = args.intVal(0, "concurrency"); err != nil {
		return a.die("%v", err)
	}
	if o.MinSuccessful, err = args.intVal(0, "min-successful"); err != nil {
		return a.die("%v", err)
	}
	if o.ReviewTimeout, err = args.duration(a.cfg.ReviewTimeout, "review-timeout"); err != nil {
		return a.die("%v", err)
	}
	o.Model = args.str(o.Model, "model")
	o.Effort = args.str(o.Effort, "effort")
	o.BaseBranch = args.str("", "base")
	o.ResumeRun = args.str("", "resume-run")
	o.KeepWorktree = args.boolean("keep-worktree")
	o.AllowEnvrcChange = args.boolean("allow-envrc-change")
	if args.boolean("dry-run") {
		o.Post = false
	}
	if args.boolean("no-direnv") {
		o.UseDirenv = false
	}
	if o.UseDirenv && t.Direnv == "" {
		return a.die("direnv is not installed; rerun with --no-direnv")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	notify := a.cfg.Notify && !args.boolean("no-notify")
	rep := &termReporter{out: a.out, notify: notify}
	rep.status = a.out.Status()
	rep.panel = a.out.Panel()
	live := review.TrackLive(rep, a.p.ManualDir)
	defer live.Close()

	runner := &review.Runner{
		GH: a.newGH(t.GH), Git: a.newGit(t.Git), Rep: live,
	}
	a.out.Printf("%s\n", a.out.Bold("quorum "+a.version))

	started := time.Now()
	res, err := runner.Run(ctx, o)
	if err != nil {
		rep.clear()
		a.logRun(rep.historyRun(repo, started, history.Failed, err.Error(), nil))
		if notify {
			a.out.Notify("quorum: review failed", reviewTarget(rep.number, number)+" stopped")
		}
		return a.reviewExit(err)
	}
	number = res.Findings.PR
	a.logRun(rep.historyRun(repo, started, history.OK, "", res))
	if notify {
		rep.mu.Lock()
		branch := rep.branch
		rep.mu.Unlock()
		target := fmt.Sprintf("PR #%d", number)
		if number == 0 {
			target = "Branch " + branch
		}
		body := fmt.Sprintf("%s: %d blockers, %d critical.",
			target, res.Findings.Blockers, res.Findings.Critical)
		if res.Posted {
			body += " Comment posted."
		} else {
			body += " Report written to disk."
		}
		a.out.Notify("quorum: review complete", body)
	}
	return exitOK
}

// historyRun describes this run for the history log. A run that never got as
// far as resolving its pull request has nothing to identify it by, and an
// entry that names no pull request is worse than no entry, so it reports one
// with an empty key and logRun drops it.
func (t *termReporter) historyRun(repo string, started time.Time, outcome, reason string, res *review.Result) history.Run {
	t.mu.Lock()
	number, title := t.number, t.title
	if t.repo != "" {
		repo = t.repo
	}
	t.mu.Unlock()
	if number == 0 || repo == "" {
		return history.Run{}
	}
	run := history.Run{
		Key:       fmt.Sprintf("%s#%d", repo, number),
		Title:     title,
		Kind:      history.KindReview,
		Source:    history.SourceManual,
		Outcome:   outcome,
		Reason:    reason,
		StartedAt: started,
		EndedAt:   time.Now(),
	}
	if res != nil {
		run.Reviewed = true
		run.Blockers = res.Findings.Blockers
		run.Critical = res.Findings.Critical
		run.Suggestions = res.Findings.Suggestions
		run.Questions = res.Findings.Questions
		run.CommentURL = res.CommentURL
		run.RunDir = res.RunDir
	}
	return run
}

// reviewExit maps a failure onto the exit code pr-codex-review used, so
// anything scripted around those keeps working.
func (a *app) reviewExit(err error) int {
	fmt.Fprintf(os.Stderr, "quorum: %v\n", err)
	switch {
	case errors.Is(err, review.ErrEnvrcChanged):
		return 2
	case errors.Is(err, review.ErrHeadDrifted):
		return 3
	case errors.Is(err, review.ErrAggregatorInvalid):
		return 4
	default:
		return exitError
	}
}

func (a *app) reviewUsage() {
	fmt.Printf(`Usage:
  quorum review [pr-number|github-pr-url] [options]

Reviews an open PR and posts the result. Without a PR argument, quorum uses the
open PR for the current branch when one exists. Otherwise it reviews the pushed
branch against the repository default branch and writes the report without
posting.

Options:
  -n, --runs N             Number of Codex reviewer passes. Default: %d
  --concurrency N          Max reviewer passes at once. Default: same as --runs
  --model MODEL            Model for reviewers and aggregator. Default: %s
  --effort LEVEL           minimal, low, medium, high, xhigh. Default: %s
  --base BRANCH            Base branch. Default: PR base or repository default
  --dry-run                Write the report to disk without posting it
  --keep-worktree          Keep the temporary worktree after a successful run
  --resume-run DIR         Reuse a run directory and only aggregate/publish
  --review-timeout DUR     Kill a reviewer that runs too long. Default: %s
  --min-successful N       Reviewer outputs required. Default: a majority
  --no-direnv              Skip direnv
  --allow-envrc-change     Allow direnv allow when the target changed .envrc
  --no-notify              No terminal notification when the run finishes
  -h, --help               Show this help

Exit codes:
  2  refused: the target changes an .envrc
  3  refused: the target head moved during the review
  4  the aggregator could not produce a valid report
`, a.cfg.Reviewers, a.cfg.ReviewModel, a.cfg.ReviewEffort,
		durationText(a.cfg.ReviewTimeout))
}

// reviewerState is what the live panel knows about one reviewer pass.
type reviewerState struct {
	started  time.Time
	elapsed  time.Duration
	exitCode int
	done     bool
	failed   bool
	logPath  string
}

// termReporter renders a review run to a terminal.
//
// While the passes run it keeps one line per reviewer, redrawn in place. Six
// Codex processes can run for the better part of an hour, and a single counter
// cannot say whether they are all working, whether one is far behind the rest,
// or which one died. Without a terminal the panel draws nothing and the same
// facts arrive as permanent lines instead.
type termReporter struct {
	out    *ui.Writer
	status *ui.Status
	panel  *ui.Panel
	notify bool
	number int
	runs   int
	title  string
	repo   string
	branch string

	mu       sync.Mutex
	tick     int
	reviewer map[int]*reviewerState
}

func (t *termReporter) Header(h review.RunHeader) {
	t.mu.Lock()
	t.number = h.Number
	t.runs = h.Runs
	t.title = h.Title
	t.repo = h.Repo
	t.branch = h.Branch
	t.reviewer = map[int]*reviewerState{}
	t.mu.Unlock()

	o := t.out
	o.Rule()
	if h.BranchOnly {
		o.Row("branch", o.Bold(h.Branch)+o.Dim("  ·  no open PR"))
		o.Row("repo", h.Repo)
	} else {
		o.Row("pr", o.Bold(fmt.Sprintf("#%d %s", h.Number, h.Title))+draftTag(o, h.Draft))
		o.Row("repo", h.Repo+o.Dim("  ·  @"+h.Author))
	}
	o.Row("base", fmt.Sprintf("%s %s", h.BaseRef, o.Dim(shortSHA(h.BaseSHA))))
	o.Row("head", o.Dim(shortSHA(h.HeadSHA)))
	o.Row("reviewers", fmt.Sprintf("%d %s", h.Runs,
		o.Dim(fmt.Sprintf("· %s · effort %s · concurrency %d · timeout %s",
			h.Model, h.Effort, h.Concurrency, durationText(h.Timeout)))))
	// The path is long, always absolute and rarely typed out: the name is
	// enough to recognise the run, and the link opens the rest.
	o.Row("run dir", o.Link(o.Dim(filepath.Base(h.RunDir)), "file://"+h.RunDir))
	o.Rule()
}

// draftTag marks a draft. A pull request that is not a draft says nothing,
// because "draft false" is a row that is only ever noise.
func draftTag(w *ui.Writer, draft bool) string {
	if !draft {
		return ""
	}
	return w.Dim("  draft")
}

// shortSHA is the seven characters git itself shows.
func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

func (t *termReporter) ReviewerStarted(idx int) {
	t.mu.Lock()
	t.reviewer[idx] = &reviewerState{started: time.Now()}
	t.mu.Unlock()
}

// draw repaints the reviewer panel.
func (t *termReporter) draw(done, failed, running int, elapsed time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if !t.out.Color {
		return
	}
	now := time.Now()
	lines := make([]string, 0, t.runs+2)
	for idx := 1; idx <= t.runs; idx++ {
		lines = append(lines, "  "+t.reviewerLine(idx, now))
	}

	bar := t.out.Bar(done+failed, t.runs, 16)
	summary := fmt.Sprintf("%d/%d done", done, t.runs)
	if failed > 0 {
		summary += fmt.Sprintf(" · %s", t.out.Red(fmt.Sprintf("%d failed", failed)))
	}
	if running > 0 {
		summary += fmt.Sprintf(" · %d running", running)
	}
	lines = append(lines, "", fmt.Sprintf("  %s %s %s  %s",
		t.out.Cyan(ui.Spinner(t.tick)), bar, summary, t.out.Dim(ui.Duration(elapsed))))
	t.tick++
	t.panel.Draw(lines)
}

// reviewerLine is one row of the panel. Queued passes are named too, so the
// list always has as many rows as the run has reviewers and never reflows.
func (t *termReporter) reviewerLine(idx int, now time.Time) string {
	o := t.out
	name := fmt.Sprintf("reviewer-%d", idx)
	s := t.reviewer[idx]
	switch {
	case s == nil:
		return o.Dim("· "+ui.Pad(name, 13)) + o.Dim("queued")
	case s.failed:
		return o.Red(o.SymFail()+" ") + ui.Pad(name, 13) +
			o.Red(fmt.Sprintf("exit %d", s.exitCode)) +
			o.Dim(fmt.Sprintf(" after %s  ", ui.Duration(s.elapsed))) +
			o.Link(o.Blue("log ↗"), "file://"+s.logPath)
	case s.done:
		return o.Green(o.SymOK()+" ") + ui.Pad(name, 13) + o.Dim(ui.Duration(s.elapsed))
	default:
		return o.Cyan(ui.Spinner(t.tick)+" ") + ui.Pad(name, 13) +
			o.Dim(ui.Duration(now.Sub(s.started)))
	}
}

func reviewTarget(resolved, requested int) string {
	if resolved > 0 {
		return fmt.Sprintf("PR #%d", resolved)
	}
	if requested > 0 {
		return fmt.Sprintf("PR #%d", requested)
	}
	return "current branch PR"
}

func resolveReviewNumber(positionals []string, repo string) (int, error) {
	if len(positionals) == 0 {
		return 0, nil
	}
	if len(positionals) > 1 {
		return 0, fmt.Errorf("expected at most one PR argument")
	}
	number, argRepo, err := resolvePRArg(positionals[0], repo)
	if err != nil {
		return 0, err
	}
	if argRepo != repo {
		return 0, fmt.Errorf("PR URL is for %s, but the current checkout is %s", argRepo, repo)
	}
	return number, nil
}

func (t *termReporter) Info(s string) {
	t.clear()
	t.out.Printf("%s\n", t.out.Dim(s))
}

func (t *termReporter) Warn(s string) {
	t.clear()
	fmt.Fprintf(os.Stderr, "warning: %s\n", s)
}

// clear takes both transient displays down before anything permanent is
// printed, so a message never lands in the middle of the reviewer list.
//
// It holds the same lock the redraw does. The panel is placed by moving the
// cursor up as many lines as it last drew, so a write from another goroutine
// landing between the erase and the next frame would leave that count pointing
// at the wrong row and smear the panel across the screen.
func (t *termReporter) clear() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.clearLocked()
}

func (t *termReporter) clearLocked() {
	t.status.Clear()
	t.panel.Clear()
}

func (t *termReporter) Progress(done, failed, running int, elapsed time.Duration) {
	t.draw(done, failed, running, elapsed)
}

func (t *termReporter) ReviewerDone(idx int, elapsed time.Duration, rank, total int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reviewer[idx] = &reviewerState{elapsed: elapsed, done: true}
	if !t.out.Color {
		// No terminal, so no panel: the same fact has to reach the log as a
		// line of its own.
		t.out.Printf("%s reviewer-%d done in %s [%d/%d]\n",
			t.out.SymOK(), idx, ui.Duration(elapsed), rank, total)
	}
}

func (t *termReporter) ReviewerFailed(idx, code int, elapsed time.Duration, logPath string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.reviewer[idx] = &reviewerState{elapsed: elapsed, failed: true, exitCode: code, logPath: logPath}
	// A failed reviewer goes to stderr whatever the terminal can do: it is the
	// one event here a caller may want to catch without the rest of the run.
	// The panel comes down first and the next tick redraws it underneath, so
	// the permanent line does not land inside the block it is placed against.
	t.clearLocked()
	fmt.Fprintf(os.Stderr, "%s reviewer-%d failed with exit %d after %s -> %s\n",
		t.out.SymFail(), idx, code, ui.Duration(elapsed), logPath)
}

func (t *termReporter) Aggregating(reviewers, attempt int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	// The finished list stays on screen: which reviewers came back is what the
	// aggregation is about to work from.
	t.panel.Freeze()
	label := fmt.Sprintf("aggregating %d reviewer output(s)", reviewers)
	if attempt > 1 {
		label += fmt.Sprintf(", attempt %d", attempt)
	}
	t.status.Draw(label)
}

// Comment renders the finished comment, through glow when it is available.
func (t *termReporter) Comment(path string) {
	t.clear()
	fmt.Println()
	if t.out.Color {
		if glow, err := exec.LookPath("glow"); err == nil {
			cmd := exec.Command(glow, path)
			cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
			if cmd.Run() == nil {
				return
			}
		}
	}
	if data, err := os.ReadFile(path); err == nil {
		fmt.Println(string(data))
	}
}

func (t *termReporter) Done(res review.Result) {
	o := t.out
	f := res.Findings
	summary := f.Summary()
	if f.Blocking() > 0 {
		summary = o.Red(summary)
	} else {
		summary = o.Green(summary)
	}

	fmt.Fprintln(o.Out)
	o.Rule()
	if f.ReviewersSucceeded == f.ReviewersRequested {
		o.Row("reviewers", o.Green(fmt.Sprintf("%d/%d succeeded", f.ReviewersSucceeded, f.ReviewersRequested)))
	} else {
		o.Row("reviewers", fmt.Sprintf("%d/%d succeeded %s", f.ReviewersSucceeded, f.ReviewersRequested,
			o.Red(fmt.Sprintf("(%d failed)", f.ReviewersRequested-f.ReviewersSucceeded))))
	}
	o.Row("findings", summary)
	o.Row("duration", ui.Duration(res.Duration))
	o.Row("comment", o.Link(o.Dim(filepath.Base(res.CommentFile)), "file://"+res.CommentFile))
	if res.Posted {
		o.Row("posted", o.Green("yes")+"  "+o.Link(o.Blue(res.CommentURL), res.CommentURL))
	} else {
		o.Row("posted", o.Dim("no, dry run"))
	}
	o.Rule()
}

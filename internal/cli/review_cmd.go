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

	"github.com/yungweng/quorum/internal/automerge"
	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/engine"
	"github.com/yungweng/quorum/internal/history"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/ui"
	"github.com/yungweng/quorum/internal/usagelimit"
)

var reviewBoolFlags = map[string]bool{
	"dry-run": true, "keep-worktree": true, "no-direnv": true,
	"allow-envrc-change": true, "no-notify": true, "h": true, "help": true,
	// Retired no-ops, accepted so old invocations do not break. They describe
	// the default behaviour now.
	"post": true, "cleanup": true, "allow-base-drift": true,
}

var reviewFlags = map[string]bool{
	"n": true, "runs": true, "concurrency": true, "engine": true, "model": true,
	"effort": true, "base": true, "review-timeout": true, "min-successful": true,
	"resume-run": true,
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
		Engine:         a.cfg.ReviewEngine,
		Model:          a.cfg.ReviewModel,
		Effort:         a.cfg.ReviewEffort,
		Runs:           a.cfg.Reviewers,
		Post:           a.cfg.Post,
		UseDirenv:      t.Direnv != "",
		RunsDir:        a.p.ReviewRuns,
		SharedRunsDirs: []string{a.p.BabysitRuns},
		DepsDir:        a.p.DepsCache,
		CodexBin:       t.Codex,
		ClaudeBin:      t.Claude,
		GrokBin:        t.Grok,
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
	o.Engine = args.str(o.Engine, "engine")
	if _, err := engineBinary(o.Engine, t, "--engine/REVIEW_ENGINE"); err != nil {
		return a.die("%v", err)
	}
	o.Model = args.str(o.Model, "model")
	// The configured effort is kept only if the engine this run resolved to
	// accepts it, because --engine can point at the other engine's set of
	// levels. One passed on the command line is not dropped: it fails.
	o.Effort = args.str(engine.KnownEffort(o.Engine, o.Effort), "effort")
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
	if o.Rules, err = config.RepoRules(a.p.RulesDir, o.Repo); err != nil {
		return a.die("reading the review rules for %s: %v", o.Repo, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	notify := a.cfg.Notify && !args.boolean("no-notify")
	rep := &termReporter{out: a.out, notify: notify}
	rep.status = a.out.Status()
	rep.panel = a.out.Panel()
	live := review.TrackLive(rep, a.p.ManualDir)
	defer live.Close()

	client := a.newGH(t.GH)
	runner := &review.Runner{
		GH: client, Git: a.newGit(t.Git), Rep: live,
	}
	a.out.Printf("%s\n", a.out.Bold("quorum "+a.version))

	started := time.Now()
	// A finished run is the only thing that grows the cache, so the next
	// collector has to measure instead of trusting a size from before it.
	defer a.forgetCacheSize()
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
	mergeStatus := ""
	if automerge.Allowed(a.cfg.AutoMerge, a.cfg.Post, res.Findings) {
		mergeResult, mergeErr := a.autoMerge(ctx, client, repoRoot, repo, number, res.Findings.HeadSHA)
		if mergeErr != nil {
			// The review finished and was posted. Keep that result in OPEN;
			// the merge error remains the reason and the command still fails.
			a.logRun(rep.historyRun(repo, started, history.OK, mergeErr.Error(), res))
			if notify {
				a.out.Notify("quorum: auto-merge failed", fmt.Sprintf("PR #%d: %s", number, mergeErr))
			}
			return a.reviewExit(mergeErr)
		}
		mergeStatus = mergeResult.Status
		if mergeStatus == automerge.ApprovalRequired {
			a.out.Printf("auto-merge: %s\n", a.out.Yellow(mergeStatus))
			a.notifyApprovalRequired(notify, repo, number, "")
		} else {
			a.out.Printf("auto-merge: %s\n", a.out.Green(mergeStatus))
		}
	}
	a.logRun(rep.historyRun(repo, started, history.OK, "", res))
	if notify && a.cfg.NotifyReadyToMerge && mergeStatus == "" && automerge.Eligible(res.Findings) {
		a.notifyReadyToMerge(notify, repo, number, "")
		return exitOK
	}
	if notify && mergeStatus != automerge.ApprovalRequired {
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
		switch mergeStatus {
		case automerge.Merged:
			body += " Merged."
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
	number, title, author, branch := t.number, t.title, t.author, t.branch
	if t.repo != "" {
		repo = t.repo
	}
	t.mu.Unlock()
	if repo == "" || number == 0 && branch == "" {
		return history.Run{}
	}
	key := fmt.Sprintf("%s#%d", repo, number)
	historyBranch := ""
	if number == 0 {
		key = history.BranchKey(repo, branch)
		historyBranch = branch
	}
	run := history.Run{
		Key:       key,
		Branch:    historyBranch,
		Title:     title,
		Author:    author,
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
	case errors.Is(err, review.ErrAggregatorInvalid), errors.Is(err, review.ErrReviewerInvalid), errors.Is(err, review.ErrVerifierInvalid):
		return 4
	case errors.Is(err, usagelimit.Err):
		return exitUsageLimit
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
  -n, --runs N             Number of reviewer passes. Default: %d
  --concurrency N          Max reviewer passes at once. Default: same as --runs
  --engine ENGINE          codex, claude or grok. Default: %s
  --model MODEL            Model for reviewers, aggregator and verifier.
                           Default: the engine's own (%s / %s / %s)
  --effort LEVEL           codex: minimal, low, medium, high, xhigh, max, ultra
                           claude: low, medium, high, xhigh, max
                           grok: low, medium, high
                           Default: %s
  --base BRANCH            Base branch. Default: PR base or repository default
  --dry-run                Write the report to disk without posting it
  --keep-worktree          Keep the temporary worktree after a successful run
  --resume-run DIR         Reuse a run with its original target and base
  --review-timeout DUR     Kill a reviewer that runs too long. Default: %s
  --min-successful N       Reviewer outputs required. Default: a majority
  --no-direnv              Skip direnv
  --allow-envrc-change     Allow direnv allow when the target changed .envrc
  --no-notify              Disable completion and action notifications
  -h, --help               Show this help

Exit codes:
  2  refused: the target changes an .envrc
  3  refused: the target head moved during the review
  4  the aggregator or verifier could not produce a valid report
  8  the engine refused: its usage limit is exhausted
`, a.cfg.Reviewers, orText(a.cfg.ReviewEngine, "codex"),
		review.DefaultModel, review.ClaudeDefaultModel, review.GrokDefaultModel,
		effortDefaultText(a.cfg.ReviewEngine, a.cfg.ReviewEffort),
		durationText(a.cfg.ReviewTimeout))
}

// orText substitutes fallback for an empty configured value in help text.
func orText(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

// modelDefaultText and effortDefaultText name what a review-style pass would
// use when the flag is omitted. Both depend on the configured engine: only
// Codex has an opinionated default for either, so quoting Codex's numbers at a
// Claude run would describe a setting that never applies.
func modelDefaultText(eng, configured string) string {
	_, model, _ := review.ReviewEngineDefaults(eng, configured, "")
	return model
}

func effortDefaultText(eng, configured string) string {
	_, _, effort := review.ReviewEngineDefaults(eng, "", configured)
	return orText(effort, "the engine's own")
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
	author string
	repo   string
	branch string
	model  engine.Model

	mu       sync.Mutex
	tick     int
	reviewer map[int]*reviewerState
}

func (t *termReporter) Header(h review.RunHeader) {
	t.mu.Lock()
	t.number = h.Number
	t.runs = h.Runs
	t.title = h.Title
	t.author = h.Author
	t.repo = h.Repo
	t.branch = h.Branch
	t.model = engine.Model{Engine: h.Engine, Name: h.Model, Effort: h.Effort}
	t.reviewer = map[int]*reviewerState{}
	t.mu.Unlock()

	o := t.out
	o.Rule()
	if h.BranchOnly {
		o.Row("branch", o.Bold(h.Branch)+o.Dim("  ·  no open PR"))
		o.Row("repo", h.Repo)
	} else {
		o.Row("pr", o.Link(o.Bold(fmt.Sprintf("#%d %s", h.Number, h.Title)), h.URL)+draftTag(o, h.Draft))
		// The URL is spelled out as well: the OSC 8 link above is invisible in
		// terminals without hyperlink support and cannot be copied anywhere.
		if h.URL != "" {
			o.Row("url", o.Link(o.Blue(h.URL), h.URL))
		}
		o.Row("repo", h.Repo+o.Dim("  ·  @"+h.Author))
	}
	o.Row("base", fmt.Sprintf("%s %s", h.BaseRef, o.Dim(shortSHA(h.BaseSHA))))
	o.Row("head", o.Dim(shortSHA(h.HeadSHA)))
	// The header carries the resolved model and effort. Claude is left on its
	// own effort unless one is configured, and an empty one here would print a
	// blank where a level belongs.
	o.Row("reviewers", fmt.Sprintf("%d %s", h.Runs,
		o.Dim(fmt.Sprintf("· %s · effort %s · concurrency %d · timeout %s",
			h.Model, orText(h.Effort, engineName(h.Engine)+"'s own"),
			h.Concurrency, durationText(h.Timeout)))))
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
	t.out.Printf("%s\n", t.out.Yellow(t.out.SymWarn()+" "+s))
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
	t.status.Draw(label + " · " + t.model.Tag())
}

func (t *termReporter) Verifying(attempt int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.panel.Freeze()
	label := "verifying findings"
	if attempt > 1 {
		label += fmt.Sprintf(", attempt %d", attempt)
	}
	t.status.Draw(label + " · " + t.model.Tag())
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
	// The header said this too, an hour and a full comment ago. Repeating it
	// here is what puts it in reach of a piped log and of scrollback.
	o.Row("model", o.Dim(t.model.Tag()))
	o.Row("duration", ui.Duration(res.Duration))
	o.Row("comment", o.Link(o.Dim(filepath.Base(res.CommentFile)), "file://"+res.CommentFile))
	o.Row("verification", o.Link(o.Dim(filepath.Base(res.VerificationChangesFile)), "file://"+res.VerificationChangesFile))
	if res.Posted {
		o.Row("posted", o.Green("yes")+"  "+o.Link(o.Blue(res.CommentURL), res.CommentURL))
	} else {
		o.Row("posted", o.Dim("no, dry run"))
	}
	o.Rule()
}

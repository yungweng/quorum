package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

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
	if len(args.pos) != 1 {
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
	number, argRepo, err := resolvePRArg(args.pos[0], repo)
	if err != nil {
		return a.die("%v", err)
	}
	if argRepo != repo {
		return a.die("PR URL is for %s, but the current checkout is %s", argRepo, repo)
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
	live := review.TrackLive(rep, a.p.ManualDir)
	defer live.Close()

	runner := &review.Runner{
		GH: a.newGH(t.GH), Git: a.newGit(t.Git), Rep: live,
	}
	a.out.Printf("%s\n", a.out.Bold("quorum "+a.version))

	res, err := runner.Run(ctx, o)
	if err != nil {
		rep.status.Clear()
		if notify {
			a.out.Notify("quorum: review failed", fmt.Sprintf("PR #%d stopped", number))
		}
		return a.reviewExit(err)
	}
	if notify {
		body := fmt.Sprintf("PR #%d: %d blockers, %d critical.",
			number, res.Findings.Blockers, res.Findings.Critical)
		if res.Posted {
			body += " Comment posted."
		} else {
			body += " Dry run complete."
		}
		a.out.Notify("quorum: review complete", body)
	}
	return exitOK
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
  quorum review <pr-number|github-pr-url> [options]

Options:
  -n, --runs N             Number of Codex reviewer passes. Default: %d
  --concurrency N          Max reviewer passes at once. Default: same as --runs
  --model MODEL            Model for reviewers and aggregator. Default: %s
  --effort LEVEL           minimal, low, medium, high, xhigh. Default: %s
  --base BRANCH            Base branch to review against. Default: the PR base
  --dry-run                Write the comment to disk without posting it
  --keep-worktree          Keep the temporary worktree after a successful run
  --resume-run DIR         Reuse a run directory and only aggregate/post
  --review-timeout DUR     Kill a reviewer that runs too long. Default: %s
  --min-successful N       Reviewer outputs required. Default: a majority
  --no-direnv              Skip direnv
  --allow-envrc-change     Allow direnv allow even when the PR changed .envrc
  --no-notify              No terminal notification when the run finishes
  -h, --help               Show this help

Exit codes:
  2  refused: the PR changes an .envrc
  3  refused: the PR head moved during the review
  4  the aggregator could not produce a postable comment
`, a.cfg.Reviewers, a.cfg.ReviewModel, a.cfg.ReviewEffort,
		durationText(a.cfg.ReviewTimeout))
}

// termReporter renders a review run to a terminal.
type termReporter struct {
	out    *ui.Writer
	status *ui.Status
	notify bool
	runs   int
}

func (t *termReporter) Header(h review.RunHeader) {
	t.runs = h.Runs
	o := t.out
	o.Rule()
	o.Row("PR", o.Bold(fmt.Sprintf("#%d %s", h.Number, h.Title)))
	o.Row("Author", "@"+h.Author)
	o.Row("Repo", h.Repo)
	o.Row("Base", fmt.Sprintf("%s (GitHub reported %s)", h.BaseRef, h.BaseSHA))
	o.Row("Head", h.HeadSHA)
	o.Row("Draft", fmt.Sprint(h.Draft))
	o.Row("Reviewers", fmt.Sprintf("%d (concurrency %d, timeout %s)",
		h.Runs, h.Concurrency, durationText(h.Timeout)))
	o.Row("Model", h.Model+" (reviewers + aggregator)")
	o.Row("Effort", h.Effort)
	o.Row("Run dir", h.RunDir)
	o.Rule()
}

func (t *termReporter) Info(s string) {
	t.status.Clear()
	t.out.Printf("%s\n", t.out.Dim(s))
}

func (t *termReporter) Warn(s string) {
	t.status.Clear()
	fmt.Fprintf(os.Stderr, "warning: %s\n", s)
}

func (t *termReporter) Progress(done, failed, running int, elapsed time.Duration) {
	t.status.Spin(fmt.Sprintf("%d done · %d failed · %d running", done, failed, running), elapsed)
}

func (t *termReporter) ReviewerDone(idx int, elapsed time.Duration, rank, total int) {
	t.status.Clear()
	t.out.Printf("%s %s\n",
		t.out.Green(fmt.Sprintf("%s reviewer-%d done in %s", t.out.SymOK(), idx, ui.Duration(elapsed))),
		t.out.Dim(fmt.Sprintf("[%d/%d]", rank, total)))
}

func (t *termReporter) ReviewerFailed(idx, code int, elapsed time.Duration, logPath string) {
	t.status.Clear()
	fmt.Fprintf(os.Stderr, "%s\n", t.out.Red(fmt.Sprintf("%s reviewer-%d failed with exit %d after %s -> %s",
		t.out.SymFail(), idx, code, ui.Duration(elapsed), logPath)))
}

func (t *termReporter) Aggregating(reviewers, attempt int) {
	t.status.Clear()
	label := fmt.Sprintf("aggregating %d reviewer output(s)", reviewers)
	if attempt > 1 {
		label += fmt.Sprintf(", attempt %d", attempt)
	}
	t.status.Draw(label)
}

// Comment renders the finished comment, through glow when it is available.
func (t *termReporter) Comment(path string) {
	t.status.Clear()
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

	o.Rule()
	if f.ReviewersSucceeded == f.ReviewersRequested {
		o.Row("Reviewers", o.Green(fmt.Sprintf("%d/%d succeeded", f.ReviewersSucceeded, f.ReviewersRequested)))
	} else {
		o.Row("Reviewers", fmt.Sprintf("%d/%d succeeded %s", f.ReviewersSucceeded, f.ReviewersRequested,
			o.Red(fmt.Sprintf("(%d failed)", f.ReviewersRequested-f.ReviewersSucceeded))))
	}
	o.Row("Findings", summary)
	o.Row("Duration", ui.Duration(res.Duration))
	o.Row("Comment", res.CommentFile)
	if res.Posted {
		o.Row("Posted", o.Green("yes")+" -> "+res.CommentURL)
	} else {
		o.Row("Posted", "no (dry run)")
	}
	o.Rule()
}

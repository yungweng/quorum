package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yungweng/quorum/internal/loop"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/ui"

	"golang.org/x/term"
)

var babysitBoolFlags = map[string]bool{
	"sandboxed": true, "interactive": true, "verbose": true, "no-notify": true,
	"no-direnv": true, "keep-worktree": true, "h": true, "help": true,
}

var babysitValueFlags = map[string]bool{
	"model": true, "effort": true, "reviewers": true, "review-model": true,
	"review-effort": true, "max-iter": true, "max-ci-fixes": true, "fix-timeout": true,
}

func (a *app) cmdBabysit(argv []string) int {
	allowed := map[string]bool{}
	for k := range babysitBoolFlags {
		allowed[k] = true
	}
	for k := range babysitValueFlags {
		allowed[k] = true
	}

	args, err := parseArgs(argv, babysitBoolFlags)
	if err != nil {
		return a.die("%v", err)
	}
	if args.boolean("h", "help") {
		a.babysitUsage()
		return exitOK
	}
	if bad := args.unknown(allowed); len(bad) > 0 {
		return a.die("unknown option: --%s", bad[0])
	}

	t, err := a.findTools()
	if err != nil {
		return a.die("%v", err)
	}
	repoRoot, repo, err := a.hereRepo(t)
	if err != nil {
		return a.die("%v", err)
	}

	// The first positional that looks like a PR is the PR; everything else is
	// extra context for the fix session, which is how babysit behaved.
	number, extraContext, err := resolveBabysitTarget(args.pos, repo)
	if err != nil {
		return a.die("%v", err)
	}

	o := loop.Options{
		Repo: repo, RepoRoot: repoRoot, Number: number,
		Context:       strings.Join(extraContext, " "),
		Model:         a.cfg.FixModel,
		Effort:        a.cfg.FixEffort,
		Reviewers:     a.cfg.Reviewers,
		ReviewModel:   a.cfg.ReviewModel,
		ReviewEffort:  a.cfg.ReviewEffort,
		Bypass:        !a.cfg.Sandboxed,
		UseDirenv:     t.Direnv != "",
		RunsDir:       a.p.BabysitRuns,
		ReviewRunsDir: a.p.ReviewRuns,
		DepsDir:       a.p.DepsCache,
		CodexBin:      t.Codex,
		DirenvBin:     t.Direnv,
	}
	if o.MaxIter, err = args.intVal(a.cfg.MaxIter, "max-iter"); err != nil {
		return a.die("%v", err)
	}
	if o.MaxCIFixes, err = args.intVal(a.cfg.MaxCIFixes, "max-ci-fixes"); err != nil {
		return a.die("%v", err)
	}
	if o.Reviewers, err = args.intVal(o.Reviewers, "reviewers"); err != nil {
		return a.die("%v", err)
	}
	if o.FixTimeout, err = args.duration(a.cfg.FixTimeout, "fix-timeout"); err != nil {
		return a.die("%v", err)
	}
	o.Model = args.str(o.Model, "model")
	o.Effort = args.str(o.Effort, "effort")
	o.ReviewModel = args.str(o.ReviewModel, "review-model")
	o.ReviewEffort = args.str(o.ReviewEffort, "review-effort")
	o.Interactive = args.boolean("interactive")
	o.Verbose = args.boolean("verbose")
	o.Out = os.Stdout
	o.KeepWorktree = args.boolean("keep-worktree")
	if args.boolean("sandboxed") {
		o.Bypass = false
	}
	if args.boolean("no-direnv") {
		o.UseDirenv = false
	}
	if o.UseDirenv && t.Direnv == "" {
		return a.die("direnv is not installed; rerun with --no-direnv")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	rep := &loopTermReporter{
		out:     a.out,
		notify:  a.cfg.Notify && !args.boolean("no-notify"),
		verbose: args.boolean("verbose"),
	}
	rep.status = a.out.Status()

	pipe := &loop.Pipeline{
		GH:     a.newGH(t.GH),
		Git:    a.newGit(t.Git),
		Review: &review.Runner{GH: a.newGH(t.GH), Git: a.newGit(t.Git), Rep: review.NopReporter{}},
		Rep:    rep,
	}
	// Interactive gates need a real terminal. Handing them a closed or piped
	// stdin would make them abort on an immediate EOF, which is confusing; nil
	// makes the reason explicit instead.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		pipe.In = os.Stdin
	}

	a.out.Printf("%s\n", a.out.Bold("quorum "+a.version))
	res, err := pipe.Run(ctx, o)
	rep.status.Clear()

	if res != nil {
		rep.summary(res)
	}
	if err != nil {
		return a.babysitExit(err)
	}
	return exitOK
}

func resolveBabysitTarget(positionals []string, repo string) (int, []string, error) {
	number := 0
	var extraContext []string
	for _, positional := range positionals {
		if number == 0 {
			if n, argRepo, err := resolvePRArg(positional, repo); err == nil {
				if argRepo != repo {
					return 0, nil, fmt.Errorf("PR URL is for %s, but the current checkout is %s", argRepo, repo)
				}
				number = n
				continue
			}
		}
		extraContext = append(extraContext, positional)
	}
	return number, extraContext, nil
}

// babysitExit maps a failure onto babysit's exit codes.
func (a *app) babysitExit(err error) int {
	fmt.Fprintf(os.Stderr, "quorum: %v\n", err)
	switch {
	case errors.Is(err, loop.ErrGateAborted):
		return exitGateAborted
	case errors.Is(err, loop.ErrCIRed):
		return exitCIRed
	case errors.Is(err, loop.ErrNotConverged):
		return exitNotConverged
	case errors.Is(err, loop.ErrNoProgress):
		return exitNoProgress
	default:
		return exitError
	}
}

func (a *app) babysitUsage() {
	fmt.Printf(`Usage:
  quorum babysit [options] [pr-number|pr-url] [extra context...]

Runs the review-fix cycle for an open PR until a review reports zero Blockers
and Critical findings and CI is green. Without a PR argument the PR of the
current branch is used. Extra positional text becomes context for the fix
session.

The fix sessions run with --dangerously-bypass-approvals-and-sandbox by
default: they must run tests, use gh and push, all unattended. Pass
--sandboxed to use your codex defaults instead.

Options:
  --model MODEL          Model for the fix sessions. Default: your codex default
  --effort LEVEL         minimal, low, medium, high, xhigh
  --reviewers N          Reviewer passes per review round. Default: %d
  --review-model MODEL   Model for the review rounds. Default: %s
  --review-effort LEVEL  Effort for the review rounds. Default: %s
  --max-iter N           Max review->fix rounds. Default: %d
  --max-ci-fixes N       Max CI fix attempts per green-CI phase. Default: %d
  --fix-timeout DUR      Kill a fix step that runs longer. Default: %s
  --sandboxed            Use your codex sandbox/approval defaults
  --interactive          Ask at gates instead of deciding autonomously
  --verbose              Stream the full output instead of the status line
  --no-notify            Disable terminal notifications
  --no-direnv            Skip direnv
  --keep-worktree        Keep the worktree after success
  -h, --help             Show this help

Exit codes:
  2  aborted at a gate
  3  CI still red after --max-ci-fixes attempts
  4  review not converged after --max-iter rounds
  5  a fix round produced no changes although findings remain
`, a.cfg.Reviewers, a.cfg.ReviewModel, a.cfg.ReviewEffort,
		a.cfg.MaxIter, a.cfg.MaxCIFixes, durationText(a.cfg.FixTimeout))
}

// loopTermReporter renders a pipeline run to a terminal.
type loopTermReporter struct {
	out     *ui.Writer
	status  *ui.Status
	notify  bool
	verbose bool
}

func (l *loopTermReporter) Header(h loop.Header) {
	o := l.out
	o.Rule()
	o.Row("pr", o.Bold(fmt.Sprintf("#%d %s", h.Number, h.Title)))
	o.Row("repo", h.Repo)
	o.Row("branch", h.Branch+o.Dim(" → ")+h.Base)
	o.Row("model", modelDesc(h.Model, h.Effort))
	// The sandbox line is the one thing here worth reading twice: bypassed
	// means these sessions push and run tests unattended.
	if h.Bypass {
		o.Row("sandbox", o.Yellow("bypassed")+o.Dim("  --dangerously-bypass-approvals-and-sandbox"))
	} else {
		o.Row("sandbox", "codex defaults"+o.Dim("  --sandboxed"))
	}
	mode := "autonomous"
	if h.Interactive {
		mode = "interactive" + o.Dim("  gates ask in the terminal")
	}
	o.Row("mode", mode)
	o.Row("limits", fmt.Sprintf("%d review rounds, %d CI fixes", h.MaxIter, h.MaxCIFixes)+
		o.Dim(fmt.Sprintf("  ·  %s per fix step", durationText(h.FixTimeout))))
	o.Row("run dir", o.Link(o.Dim(filepath.Base(h.RunDir)), "file://"+h.RunDir))
	o.Rule()
}

func (l *loopTermReporter) Step(title string) {
	l.status.Clear()
	l.out.Step(title)
}

func (l *loopTermReporter) StepStart(label string) {
	if !l.out.Color {
		l.out.Printf("running: %s\n", label)
	}
}

func (l *loopTermReporter) StepTick(label string, elapsed time.Duration) {
	if !l.verbose {
		l.status.Spin(label, elapsed)
	}
}

func (l *loopTermReporter) StepEnd(label string, elapsed time.Duration, ok bool) {
	l.status.Clear()
	if ok {
		l.out.Printf("%s\n", l.out.Dim(fmt.Sprintf("%s %s · %s",
			l.out.SymOK(), label, ui.Duration(elapsed))))
	}
}

func (l *loopTermReporter) RoundResult(round int, f review.Findings, clean bool) {
	l.status.Clear()
	line := f.Summary()
	if clean {
		line = l.out.Green(line)
	} else {
		line = l.out.Red(line)
	}
	l.out.Printf("%s %s\n",
		l.out.Dim(fmt.Sprintf("%s Review round %d ·", l.out.SymOK(), round)), line)
}

func (l *loopTermReporter) CIGreen() {
	l.status.Clear()
	l.out.Printf("%s\n", l.out.Green("CI green."))
}

func (l *loopTermReporter) CIRed(attempt, max int) {
	l.status.Clear()
	l.out.Printf("%s\n", l.out.Red(fmt.Sprintf("CI red; starting fix attempt %d/%d.", attempt, max)))
}

func (l *loopTermReporter) Info(s string) {
	l.status.Clear()
	l.out.Printf("%s\n", l.out.Dim(s))
}

func (l *loopTermReporter) Warn(s string) {
	l.status.Clear()
	fmt.Fprintf(os.Stderr, "warning: %s\n", s)
}

func (l *loopTermReporter) Questions(text string) { l.block(text) }
func (l *loopTermReporter) Dispute(text string)   { l.block(text) }

func (l *loopTermReporter) block(text string) {
	l.status.Clear()
	fmt.Println()
	l.out.Rule()
	fmt.Println(text)
	l.out.Rule()
}

func (l *loopTermReporter) EnvrcChanged(diff string) {
	l.status.Clear()
	fmt.Fprintf(os.Stderr, "%s\n%s\n", l.out.Red("A .envrc file changed inside the worktree."), diff)
}

func (l *loopTermReporter) Prompt(text string) {
	l.status.Clear()
	fmt.Println(text)
}

func (l *loopTermReporter) Notify(title, body string) {
	l.out.Bell()
	if l.notify {
		l.out.Notify("quorum: "+title, body)
	}
}

// summary prints the closing block of a run.
func (l *loopTermReporter) summary(res *loop.Result) {
	o := l.out
	fmt.Println()
	o.Rule()
	o.Row("pr", o.Bold(fmt.Sprintf("#%d", res.PR.Number))+"  "+o.Link(o.Blue(res.PR.URL), res.PR.URL))
	o.Row("branch", res.PR.HeadRefName)
	o.Row("rounds", fmt.Sprintf("%d review round(s)", res.Rounds))
	o.Row("duration", ui.Duration(res.Duration))
	if len(res.RoundLog) > 0 {
		fmt.Println("  Commits per round:")
		for _, r := range res.RoundLog {
			fmt.Printf("    %s:\n", r.Label)
			for _, line := range strings.Split(r.Commits, "\n") {
				fmt.Printf("      %s\n", line)
			}
		}
	}
	switch {
	case res.Converged && res.DisputeAccepted:
		o.Row("result", o.Green("CI green, remaining findings disputed by Codex and accepted"))
		fmt.Println("  Note: the review comment on the PR still lists the disputed findings.")
		if res.DisputeText != "" {
			for _, line := range strings.Split(res.DisputeText, "\n") {
				fmt.Printf("  %s\n", line)
			}
		}
		o.Rule()
		l.Notify("Fertig", fmt.Sprintf("PR #%d fertig; Disputes akzeptiert, bereit fuer den manuellen Test", res.PR.Number))
	case res.Converged:
		o.Row("result", o.Green("review clean, CI green"))
		o.Rule()
		l.Notify("Fertig", fmt.Sprintf("PR #%d ist bereit fuer den manuellen Test", res.PR.Number))
	default:
		o.Row("result", o.Red("not converged"))
		o.Rule()
	}
}

func modelDesc(model, effort string) string {
	switch {
	case model != "" && effort != "":
		return fmt.Sprintf("%s (effort %s)", model, effort)
	case model != "":
		return model
	case effort != "":
		return "codex default (effort " + effort + ")"
	default:
		return "codex default"
	}
}

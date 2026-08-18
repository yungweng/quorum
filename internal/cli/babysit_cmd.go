package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/yungweng/quorum/internal/automerge"
	"github.com/yungweng/quorum/internal/engine"
	"github.com/yungweng/quorum/internal/history"
	"github.com/yungweng/quorum/internal/loop"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/ui"
	"github.com/yungweng/quorum/internal/usagelimit"

	"golang.org/x/term"
)

var babysitBoolFlags = map[string]bool{
	"sandboxed": true, "interactive": true, "verbose": true, "no-notify": true,
	"no-direnv": true, "allow-envrc-change": true, "keep-worktree": true, "divergence-scan": true,
	"draft": true, "local": true, "no-resolve-conflicts": true,
	"h": true, "help": true,
}

var babysitValueFlags = map[string]bool{
	"engine": true, "model": true, "effort": true, "reviewers": true,
	"review-engine": true, "review-model": true, "review-effort": true,
	"max-iter": true, "max-ci-fixes": true, "fix-timeout": true,
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
	if args.boolean("local") && number > 0 {
		return a.die("--local works on the current branch; do not pass a PR")
	}

	o := loop.Options{
		Repo: repo, RepoRoot: repoRoot, Number: number,
		Context:              strings.Join(extraContext, " "),
		Engine:               a.cfg.FixEngine,
		Model:                a.cfg.FixModel,
		Effort:               a.cfg.FixEffort,
		Reviewers:            a.cfg.Reviewers,
		ReviewEngine:         a.cfg.ReviewEngine,
		ReviewModel:          a.cfg.ReviewModel,
		ReviewEffort:         a.cfg.ReviewEffort,
		Bypass:               !a.cfg.Sandboxed,
		UseDirenv:            t.Direnv != "",
		RunsDir:              a.p.BabysitRuns,
		ReviewRunsDir:        a.p.ReviewRuns,
		DepsDir:              a.p.DepsCache,
		CodexBin:             t.Codex,
		ClaudeBin:            t.Claude,
		GrokBin:              t.Grok,
		DirenvBin:            t.Direnv,
		Post:                 a.cfg.Post,
		AllowDraft:           a.cfg.BabysitDrafts || args.boolean("draft"),
		Local:                args.boolean("local"),
		ResolveConflicts:     a.cfg.ResolveConflicts && !args.boolean("no-resolve-conflicts"),
		DivergenceScan:       a.cfg.DivergenceScan || args.boolean("divergence-scan"),
		DivergenceEscalateTo: slices.Clone(a.cfg.DivergenceEscalateTo),
		DivergenceTimeout:    a.cfg.ReviewTimeout,
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
	o.Engine = args.str(o.Engine, "engine")
	o.ReviewEngine = args.str(o.ReviewEngine, "review-engine")
	if _, err := engineBinary(o.Engine, t, "--engine/FIX_ENGINE"); err != nil {
		return a.die("%v", err)
	}
	if _, err := engineBinary(o.ReviewEngine, t, "--review-engine/REVIEW_ENGINE"); err != nil {
		return a.die("%v", err)
	}
	o.Model = args.str(o.Model, "model")
	// A configured effort survives only if the engine this run resolved to
	// accepts it: --engine and --review-engine can each point at the other
	// engine's set of levels. One passed on the command line is not dropped:
	// it fails.
	o.Effort = args.str(engine.KnownEffort(o.Engine, o.Effort), "effort")
	o.ReviewModel = args.str(o.ReviewModel, "review-model")
	o.ReviewEffort = args.str(engine.KnownEffort(o.ReviewEngine, o.ReviewEffort), "review-effort")
	o.Interactive = args.boolean("interactive")
	o.Verbose = args.boolean("verbose")
	o.Out = os.Stdout
	o.KeepWorktree = args.boolean("keep-worktree")
	o.AllowEnvrcChange = args.boolean("allow-envrc-change")
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

	client := a.newGH(t.GH)
	pipe := &loop.Pipeline{
		GH:     client,
		Git:    a.newGit(t.Git),
		Review: &review.Runner{GH: client, Git: a.newGit(t.Git), Rep: review.NopReporter{}},
		Rep:    rep,
	}
	// Interactive gates need a real terminal. Handing them a closed or piped
	// stdin would make them abort on an immediate EOF, which is confusing; nil
	// makes the reason explicit instead.
	if term.IsTerminal(int(os.Stdin.Fd())) {
		pipe.In = os.Stdin
	}

	a.out.Printf("%s\n", a.out.Bold("quorum "+a.version))
	started := time.Now()
	res, err := pipe.Run(ctx, o)
	rep.status.Clear()
	mergeStatus := ""
	var mergeErr error
	if err == nil && res != nil && automerge.Allowed(a.cfg.AutoMerge, a.cfg.Post, res.LastFindings) {
		if res.PR.IsDraft {
			// The auto-merge path refuses drafts outright; a converged draft
			// run must not turn that refusal into a failure of the whole run.
			rep.Info("auto-merge: skipped, the PR is a draft")
		} else {
			mergeResult, finishErr := a.autoMerge(ctx, client, repoRoot, repo, res.PR.Number, res.LastFindings.HeadSHA)
			mergeStatus, mergeErr = mergeResult.Status, finishErr
			if mergeErr == nil && mergeStatus == automerge.ApprovalRequired {
				a.notifyApprovalRequired(rep.notify, repo, res.PR.Number)
			}
			if mergeErr != nil {
				err = mergeErr
			}
		}
	}

	if err == nil && res != nil && a.cfg.NotifyReadyToMerge && mergeStatus == "" &&
		!res.PR.IsDraft && automerge.Eligible(res.LastFindings) {
		a.notifyReadyToMerge(rep.notify, repo, res.PR.Number)
		rep.readySent = rep.notify
	}

	a.logRun(babysitHistory(repo, number, started, res, err))
	if res != nil {
		rep.summary(res, err, mergeStatus, mergeErr)
	}
	if err != nil {
		return a.babysitExit(err)
	}
	return exitOK
}

// babysitHistory describes a finished fix loop for the history log. A loop
// that stopped before it could resolve its pull request has nothing to name
// itself with, so it reports an empty key and logRun drops it.
func babysitHistory(repo string, number int, started time.Time, res *loop.Result, err error) history.Run {
	run := history.Run{
		Kind:      history.KindBabysit,
		Source:    history.SourceManual,
		StartedAt: started,
		EndedAt:   time.Now(),
		Outcome:   history.Failed,
	}
	if err != nil {
		run.Reason = err.Error()
	}
	if res != nil {
		number = res.PR.Number
		run.Title = res.PR.Title
		run.Author = res.PR.Author.Login
		if res.BranchOnly {
			run.Branch = res.PR.HeadRefName
		}
		run.Rounds = res.Rounds
		run.RunDir = res.RunDir
		// Counts only mean something once a round has actually reviewed, and
		// LastFindings is zero both for a clean review and for a loop that
		// never got that far.
		if res.Rounds > 0 {
			run.Reviewed = true
			run.Blockers = res.LastFindings.Blockers
			run.Critical = res.LastFindings.Critical
			run.Suggestions = res.LastFindings.Suggestions
			run.Questions = res.LastFindings.Questions
			if res.LastFindings.CommentURL != nil {
				run.CommentURL = *res.LastFindings.CommentURL
			}
			if res.DivergenceCommentURL != "" {
				run.CommentURL = res.DivergenceCommentURL
			}
		}
		if res.Converged {
			run.Outcome = history.Converged
			if err == nil {
				run.Reason = ""
			}
		}
	}
	if repo == "" || number == 0 && run.Branch == "" {
		return history.Run{}
	}
	if number == 0 {
		run.Key = history.BranchKey(repo, run.Branch)
	} else {
		run.Key = fmt.Sprintf("%s#%d", repo, number)
	}
	return run
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
	case errors.Is(err, loop.ErrDiverged):
		return exitDiverged
	case errors.Is(err, loop.ErrConflicts):
		return exitConflicts
	case errors.Is(err, usagelimit.Err):
		return exitUsageLimit
	default:
		return exitError
	}
}

func (a *app) babysitUsage() {
	fmt.Printf(`Usage:
  quorum babysit [options] [pr-number|pr-url] [extra context...]

Runs the review-fix cycle until a review reports zero Blockers and Critical
findings. Without a PR argument, quorum uses the open PR for the current branch
when one exists. Otherwise it works on the pushed branch and skips PR CI and PR
comments. After a posted PR run converges, a fresh read-only pass writes a local
PR-description candidate for the final state. Extra positional text becomes
context for the fix session.

Draft PRs are refused unless you pass --draft or set BABYSIT_DRAFTS=1. When the
branch conflicts with its base, the base is merged and the conflicts resolved
before the first review; RESOLVE_CONFLICTS=0 or --no-resolve-conflicts turns
that off.

The fix sessions run with the engine's sandbox and approvals bypassed by
default (codex: --dangerously-bypass-approvals-and-sandbox, claude:
--dangerously-skip-permissions, grok: --always-approve): they must run tests,
use gh and push, all unattended. Pass --sandboxed to use the engine's own
defaults instead.

Options:
  --engine ENGINE        Engine for the fix sessions: codex, claude or grok. Default: %s
  --model MODEL          Model for the fix sessions. Default: the engine's default
  --effort LEVEL         codex: minimal, low, medium, high, xhigh, max, ultra;
                         claude: low, medium, high, xhigh, max; grok: low, medium, high
  --reviewers N          Reviewer passes per review round. Default: %d
  --review-engine ENGINE Engine for the review rounds. Default: %s
  --review-model MODEL   Model for the review rounds. Default: %s
  --review-effort LEVEL  Effort for the review rounds. Default: %s
  --max-iter N           Max review->fix rounds. Default: %d
  --max-ci-fixes N       Max PR CI fix attempts per green-CI phase. Default: %d
  --fix-timeout DUR      Kill a fix step that runs longer. Default: %s
  --divergence-scan      Analyze the round history after --max-iter, then stop
  --draft                Work on a draft PR (standing default: BABYSIT_DRAFTS=1)
  --local                Ignore any open PR: review and fix the pushed branch,
                         post nothing to GitHub
  --no-resolve-conflicts Do not merge the base branch when the branch conflicts
  --sandboxed            Use the engine's own sandbox/approval defaults
  --interactive          Ask at gates instead of deciding autonomously
  --verbose              Stream the full output instead of the status line
  --no-notify            Disable completion and action notifications
  --no-direnv            Skip direnv
  --allow-envrc-change   Allow direnv allow when the target changed .envrc
  --keep-worktree        Keep the worktree after success
  -h, --help             Show this help

Exit codes:
  2  aborted at a gate
  3  CI still red after --max-ci-fixes attempts
  4  review not converged after --max-iter rounds
  5  a fix round produced no changes although findings remain
  6  the review/fix history contains incompatible decisions
  7  merge conflicts with the base branch remain unresolved
  8  the engine refused: its usage limit is exhausted
`, orText(a.cfg.FixEngine, "codex"), a.cfg.Reviewers,
		orText(a.cfg.ReviewEngine, "codex"),
		modelDefaultText(a.cfg.ReviewEngine, a.cfg.ReviewModel),
		effortDefaultText(a.cfg.ReviewEngine, a.cfg.ReviewEffort),
		a.cfg.MaxIter, a.cfg.MaxCIFixes, durationText(a.cfg.FixTimeout))
}

// loopTermReporter renders a pipeline run to a terminal.
type loopTermReporter struct {
	out     *ui.Writer
	status  *ui.Status
	notify  bool
	verbose bool
	// readySent records that the persistent ready-to-merge notification went
	// out, so the summary skips its own transient completion notification.
	readySent bool
	// pending holds a finished review round's step facts until its RoundResult
	// arrives a moment later, so the round prints as one line carrying model,
	// duration and findings instead of two lines repeating each other.
	pending struct {
		label   string
		model   engine.Model
		elapsed time.Duration
	}
	hasPending bool
}

// reviewRoundLabel matches exactly the step labels that a RoundResult follows.
// Discarded, resumed or fresh retries carry a suffix and print on their own.
var reviewRoundLabel = regexp.MustCompile(`^Review round \d+$`)

// flushPending prints a held review-round line whose RoundResult never came,
// which happens when the run fails between the review and its result.
func (l *loopTermReporter) flushPending() {
	if !l.hasPending {
		return
	}
	l.hasPending = false
	l.out.Printf("%s\n", l.out.Dim(fmt.Sprintf("%s %s · %s · %s",
		l.out.SymOK(), l.pending.label, l.pending.model.Tag(), ui.Duration(l.pending.elapsed))))
}

func (l *loopTermReporter) Header(h loop.Header) {
	o := l.out
	o.Rule()
	switch {
	case h.Local:
		o.Row("target", o.Bold(h.Branch)+o.Dim("  ·  local run, any open PR is left alone"))
	case h.BranchOnly:
		o.Row("target", o.Bold(h.Branch)+o.Dim("  ·  no open PR"))
	default:
		o.Row("pr", o.Link(o.Bold(fmt.Sprintf("#%d %s", h.Number, h.Title)), h.URL)+draftTag(o, h.Draft))
		// The URL is spelled out as well: the OSC 8 link above is invisible in
		// terminals without hyperlink support and cannot be copied anywhere.
		if h.URL != "" {
			o.Row("url", o.Link(o.Blue(h.URL), h.URL))
		}
	}
	o.Row("repo", h.Repo)
	o.Row("branch", h.Branch+o.Dim(" → ")+h.Base)
	o.Row("review model", modelDesc(h.ReviewEngine, h.ReviewModel, h.ReviewEffort))
	o.Row("fix model", modelDesc(h.Engine, h.Model, h.Effort))
	// The sandbox line is the one thing here worth reading twice: bypassed
	// means these sessions push and run tests unattended.
	if h.Bypass {
		o.Row("sandbox", o.Yellow("bypassed")+o.Dim("  unattended tests, gh and push"))
	} else {
		o.Row("sandbox", "engine defaults"+o.Dim("  --sandboxed"))
	}
	mode := "autonomous"
	if h.Interactive {
		mode = "interactive" + o.Dim("  gates ask in the terminal")
	}
	o.Row("mode", mode)
	limits := fmt.Sprintf("%d review rounds", h.MaxIter)
	if !h.BranchOnly {
		limits += fmt.Sprintf(", %d CI fixes", h.MaxCIFixes)
	}
	if h.DivergenceScan {
		limits += ", divergence report at limit"
	}
	o.Row("limits", limits+o.Dim(fmt.Sprintf("  ·  %s per fix step", durationText(h.FixTimeout))))
	o.Row("run dir", o.Link(o.Dim(filepath.Base(h.RunDir)), "file://"+h.RunDir))
	o.Rule()
}

func (l *loopTermReporter) Step(title string) {
	l.status.Clear()
	l.flushPending()
	l.out.Step(title)
}

func (l *loopTermReporter) StepStart(label string, m engine.Model) {
	if !l.out.Color {
		l.out.Printf("running: %s · %s\n", label, m.Tag())
	}
}

func (l *loopTermReporter) StepTick(label string, m engine.Model, elapsed time.Duration) {
	if !l.verbose {
		l.status.Spin(label+" · "+m.Tag(), elapsed)
	}
}

// StepEnd names the model between the step and its duration. A run has a review
// model and a fix model, the header scrolls away within the first round, and
// which one paid for an hour of wall clock is the question the line is read for.
// A review round's facts are held back and merged into its RoundResult; a
// discarded round is crossed out, so a rerun of the same number below it does
// not read as the run counting twice.
func (l *loopTermReporter) StepEnd(label string, m engine.Model, elapsed time.Duration, ok bool) {
	l.status.Clear()
	if !ok {
		return
	}
	if reviewRoundLabel.MatchString(label) {
		l.pending.label, l.pending.model, l.pending.elapsed = label, m, elapsed
		l.hasPending = true
		return
	}
	line := fmt.Sprintf("%s %s · %s · %s", l.out.SymOK(), label, m.Tag(), ui.Duration(elapsed))
	if strings.Contains(label, "(discarded)") {
		line = l.out.Strike(line)
	}
	l.out.Printf("%s\n", l.out.Dim(line))
}

func (l *loopTermReporter) RoundResult(round int, f review.Findings, clean bool) {
	l.status.Clear()
	line := f.Summary()
	if clean {
		line = l.out.Green(line)
	} else {
		line = l.out.Red(line)
	}
	prefix := fmt.Sprintf("%s Review round %d", l.out.SymOK(), round)
	if l.hasPending && l.pending.label == fmt.Sprintf("Review round %d", round) {
		l.hasPending = false
		prefix += fmt.Sprintf(" · %s · %s", l.pending.model.Tag(), ui.Duration(l.pending.elapsed))
	} else {
		l.flushPending()
	}
	l.out.Printf("%s %s\n", l.out.Dim(prefix+" ·"), line)
}

// CIWait keeps the wait on the transient status line; only a terminal-less run
// records it as a permanent line, once per wait.
func (l *loopTermReporter) CIWait(pr int, elapsed time.Duration) {
	if elapsed == 0 {
		l.flushPending()
	}
	if l.out.Color {
		l.status.Spin(fmt.Sprintf("waiting for CI on PR #%d", pr), elapsed)
		return
	}
	if elapsed == 0 {
		l.out.Printf("waiting for CI on PR #%d...\n", pr)
	}
}

func (l *loopTermReporter) CIGreen(elapsed time.Duration) {
	l.status.Clear()
	l.out.Printf("%s%s\n", l.out.Green("CI green"), l.out.Dim("  ·  waited "+ui.Duration(elapsed)))
}

func (l *loopTermReporter) CIRed(attempt, max int) {
	l.status.Clear()
	l.out.Printf("%s\n", l.out.Red(fmt.Sprintf("CI red; starting fix attempt %d/%d.", attempt, max)))
}

func (l *loopTermReporter) Info(s string) {
	l.status.Clear()
	l.flushPending()
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
func (l *loopTermReporter) summary(res *loop.Result, runErr error, mergeStatus string, mergeErr error) {
	o := l.out
	l.flushPending()
	fmt.Println()
	o.Rule()
	switch {
	case res.Local:
		o.Row("target", o.Bold(res.PR.HeadRefName)+o.Dim("  ·  local run, any open PR was left alone"))
	case res.BranchOnly:
		o.Row("target", o.Bold(res.PR.HeadRefName)+o.Dim("  ·  no open PR"))
	default:
		o.Row("pr", o.Bold(fmt.Sprintf("#%d", res.PR.Number))+"  "+o.Link(o.Blue(res.PR.URL), res.PR.URL))
	}
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
	if res.Divergence != nil {
		o.Row("analysis", res.Divergence.Verdict)
		switch {
		case res.DivergenceCommentURL != "":
			o.Row("report", o.Link(o.Blue(res.DivergenceCommentURL), res.DivergenceCommentURL))
		case res.DivergenceReportPath != "":
			o.Row("report", o.Link(o.Blue(res.DivergenceReportPath), "file://"+res.DivergenceReportPath))
		}
	}
	if res.PRDescriptionFile != "" {
		status := "generated locally"
		switch {
		case res.PRDescriptionUpdated:
			status = "updated"
		case res.PRDescriptionCurrent:
			status = "already current"
		}
		o.Row("description", o.Green(status)+"  "+o.Link(o.Dim(filepath.Base(res.PRDescriptionFile)), "file://"+res.PRDescriptionFile))
	}
	switch {
	case mergeErr != nil:
		o.Row("result", o.Red(mergeErr.Error()))
		o.Rule()
		l.Notify("Auto-merge failed", fmt.Sprintf("%s ist sauber, konnte aber nicht gemerged werden", babysitTargetLabel(res)))
	case res.Converged && res.DisputeAccepted:
		result := "remaining findings disputed by Codex and accepted"
		if !res.BranchOnly {
			result = "CI green, " + result
		}
		o.Row("result", o.Green(result))
		if !res.BranchOnly {
			switch {
			case res.DisputeCommentURL != "":
				o.Row("rebuttal", o.Link(o.Blue(res.DisputeCommentURL), res.DisputeCommentURL))
			case res.DisputeCommentPosted:
				o.Row("rebuttal", o.Green("posted"))
			default:
				fmt.Println("  Note: the rebuttal could not be posted; it is included below.")
			}
		}
		if res.DisputeText != "" {
			for _, line := range strings.Split(res.DisputeText, "\n") {
				fmt.Printf("  %s\n", line)
			}
		}
		o.Rule()
		l.Notify("Fertig", fmt.Sprintf("%s fertig; Disputes akzeptiert, bereit fuer den manuellen Test", babysitTargetLabel(res)))
	case res.Converged:
		result := "review clean"
		if !res.BranchOnly {
			result += ", CI green"
		}
		o.Row("result", o.Green(result))
		switch mergeStatus {
		case automerge.Merged:
			o.Row("auto-merge", o.Green("merged"))
		case automerge.ApprovalRequired:
			o.Row("auto-merge", o.Yellow(automerge.ApprovalRequired))
		}
		o.Rule()
		if mergeStatus != automerge.ApprovalRequired && !l.readySent {
			l.Notify("Fertig", fmt.Sprintf("%s ist bereit fuer den manuellen Test", babysitTargetLabel(res)))
		}
	case res.Divergence != nil && res.Divergence.Verdict == loop.DivergenceDiverged:
		o.Row("result", o.Red("diverged; manual decision required"))
		o.Rule()
		l.Notify("Diverged", fmt.Sprintf("%s braucht eine manuelle Designentscheidung", babysitTargetLabel(res)))
	case errors.Is(runErr, loop.ErrNotConverged):
		o.Row("result", o.Red("not converged"))
		o.Rule()
	case runErr != nil:
		o.Row("result", o.Red("failed: "+runErr.Error()))
		o.Rule()
	default:
		o.Row("result", o.Red("failed"))
		o.Rule()
	}
}

func babysitTargetLabel(res *loop.Result) string {
	if res.BranchOnly {
		return "Branch " + res.PR.HeadRefName
	}
	return fmt.Sprintf("PR #%d", res.PR.Number)
}

// modelDesc names the model and effort a phase will run on. Nothing fills in
// the fix session's model or effort, so an empty one means the engine's CLI
// decides and the row says whose choice it is rather than "engine default",
// which reads as a setting quorum knows.
func modelDesc(eng, model, effort string) string {
	own := engineName(eng) + "'s own choice"
	switch {
	case model != "" && effort != "":
		return fmt.Sprintf("%s (effort %s)", model, effort)
	case model != "":
		return model
	case effort != "":
		return fmt.Sprintf("%s (effort %s)", own, effort)
	default:
		return own
	}
}

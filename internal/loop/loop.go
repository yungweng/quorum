// Package loop drives a pull request from "implementation pushed" to "ready
// for manual testing": wait for CI, review, feed the findings into a resumable
// Codex session that fixes them, push, repeat until a review comes back with no
// Blockers and no Critical findings.
//
// Deliberately out of scope: picking issues, planning, and the first
// implementation. Those stay with the user.
package loop

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/codex"
	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/proc"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/target"
)

// Failures the CLI maps onto the exit codes the shell version defined, so
// existing scripts around babysit keep working.
var (
	// ErrGateAborted is exit 2: a user abort, a missing terminal in interactive
	// mode, or a session that keeps asking questions in autonomous mode.
	ErrGateAborted = errors.New("aborted at a gate")
	// ErrCIRed is exit 3.
	ErrCIRed = errors.New("CI still red after the allowed fix attempts")
	// ErrNotConverged is exit 4.
	ErrNotConverged = errors.New("review did not converge")
	// ErrNoProgress is exit 5: a fix round changed nothing while findings
	// remain, and did not dispute them either.
	ErrNoProgress = errors.New("fix round produced no changes although findings remain")
)

// Defaults for a run.
const (
	DefaultMaxIter    = 12
	DefaultMaxCIFixes = 3
	DefaultFixTimeout = 2 * time.Hour
	// maxQuestionBounces is how often autonomous mode sends questions back
	// before giving up rather than looping forever.
	maxQuestionBounces = 3
	// headSettleTimeout is how long GitHub gets to report a pushed sha as the
	// PR head before the pipeline treats it as a problem.
	headSettleTimeout = 180 * time.Second
	// noChecksTimeout is how long to keep asking before accepting that a PR
	// genuinely has no CI.
	noChecksTimeout = 180 * time.Second
	runRetention    = 7 * 24 * time.Hour
)

// Options configure one pipeline run.
type Options struct {
	Repo     string
	RepoRoot string
	Number   int
	Context  string // extra text handed to the fix session

	Model  string
	Effort string

	Reviewers    int
	ReviewModel  string
	ReviewEffort string

	MaxIter    int
	MaxCIFixes int
	FixTimeout time.Duration

	// Bypass runs the fix sessions with --dangerously-bypass-approvals-and-
	// sandbox. They must run tests, use gh and push, unattended, and a
	// sandboxed exec would silently skip exactly those commands.
	Bypass       bool
	Interactive  bool
	UseDirenv    bool
	KeepWorktree bool

	// Verbose streams the full Codex output instead of only the status line.
	// The logs are written either way; this decides what reaches the terminal.
	Verbose bool
	// Out is where verbose output goes. Nil discards it.
	Out io.Writer

	RunsDir       string
	ReviewRunsDir string
	DepsDir       string
	CodexBin      string
	DirenvBin     string
}

// RoundEntry is one round's commit list, for the closing summary.
type RoundEntry struct {
	Label   string
	Commits string
}

// Result is what a finished pipeline run produced.
type Result struct {
	PR              gh.FullPR
	BranchOnly      bool
	Rounds          int
	Converged       bool
	DisputeAccepted bool
	DisputeText     string
	RoundLog        []RoundEntry
	RunDir          string
	Duration        time.Duration
	LastFindings    review.Findings
}

// Pipeline runs the review-fix cycle.
type Pipeline struct {
	GH     *gh.Client
	Git    git.G
	Review *review.Runner
	Rep    Reporter
	// In supplies answers at interactive gates. Nil means no terminal, which
	// makes every interactive gate abort rather than hang.
	In io.Reader
}

// Run drives the pipeline to completion.
func (p *Pipeline) Run(ctx context.Context, o Options) (*Result, error) {
	started := time.Now()
	o = o.withDefaults()
	if err := o.validate(); err != nil {
		return nil, err
	}

	r := &run{p: p, o: o, ctx: ctx, rep: p.reporter()}
	if err := r.prepare(); err != nil {
		r.releaseRunClaim()
		return nil, err
	}
	defer r.cleanup()

	res, err := r.execute()
	if res != nil {
		res.Duration = time.Since(started)
	}
	return res, err
}

// run holds the state of one pipeline run.
type run struct {
	p   *Pipeline
	o   Options
	ctx context.Context
	rep Reporter

	target       target.Target
	pr           gh.FullPR
	branch       string
	root         string
	worktree     string
	logDir       string
	msgDir       string
	releaseClaim func()

	env   envexec.Env
	codex codex.Options
	rules string
	prCtx string

	// The fix session, shared across every CI fix and review round, so context
	// carries through the whole run.
	sessionID string
	lastMsg   string

	// stdin is created on the first interactive gate and reused, so a partly
	// consumed buffer is never thrown away between gates.
	stdin *bufio.Scanner

	direnvActive  bool
	envrcBaseline map[string]bool
	envrcStamp    string

	// A review runs in the background while the pipeline waits for CI: a review
	// only needs the pushed head, not green checks.
	review *bgReview

	roundLog        []RoundEntry
	ciFixTotal      int
	disputeAccepted bool
	disputeText     string

	// prog is what this run publishes about itself for a watcher to read. It
	// is the run's own copy; publish writes it out.
	prog Progress
}

// prepare resolves the PR or current pushed branch, checks the preconditions
// and sets up the worktree.
func (r *run) prepare() error {
	tgt, err := target.Resolve(r.ctx, r.p.GH, r.p.Git, r.o.RepoRoot, r.o.Number, "", "")
	if err != nil {
		return err
	}
	pr := tgt.PR
	if !tgt.BranchOnly && pr.State != "OPEN" {
		return fmt.Errorf("PR is %s, expected an open PR", pr.State)
	}
	// The pipeline fetches and pushes refs/heads/<branch> on origin. For a fork
	// PR that branch lives in the fork, so a same-named origin branch would be
	// reviewed and pushed instead: the wrong branch, silently.
	if !tgt.BranchOnly && pr.IsCrossRepository {
		return fmt.Errorf("PR #%d is a cross-repository (fork) PR; the pipeline can only work on branches in %s",
			pr.Number, r.o.Repo)
	}
	r.target = tgt
	r.pr = pr
	r.branch = pr.HeadRefName

	// The pipeline reviews the pushed head. Work that only exists in the user's
	// checkout would be silently ignored, so refuse rather than mislead.
	current, err := r.p.Git.CurrentBranch(r.ctx, r.o.RepoRoot)
	if err != nil {
		return err
	}
	if current == r.branch {
		dirty, err := r.p.Git.Dirty(r.ctx, r.o.RepoRoot)
		if err != nil {
			return err
		}
		if dirty {
			return fmt.Errorf("your checkout of %s has uncommitted changes; commit and push them first so the pipeline sees them", r.branch)
		}
	}

	if err := r.p.Git.Fetch(r.ctx, r.o.RepoRoot, "origin",
		fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", r.branch, r.branch)); err != nil {
		return err
	}
	headSHA, err := r.p.Git.RevParse(r.ctx, r.o.RepoRoot, "origin/"+r.branch)
	if err != nil {
		return err
	}
	if headSHA != pr.HeadRefOid {
		source := "GitHub PR metadata"
		if tgt.BranchOnly {
			source = "the branch head resolved at startup"
		}
		r.rep.Warn(fmt.Sprintf("origin/%s (%s) differs from %s (%s); using origin/%s",
			r.branch, headSHA, source, pr.HeadRefOid, r.branch))
	}
	if r.p.Git.HasLocalBranch(r.ctx, r.o.RepoRoot, r.branch) {
		localSHA, err := r.p.Git.RevParse(r.ctx, r.o.RepoRoot, "refs/heads/"+r.branch)
		if err != nil {
			return err
		}
		if localSHA != headSHA {
			return fmt.Errorf("local branch %s (%s) differs from origin (%s); push your local commits first",
				r.branch, localSHA, headSHA)
		}
	}

	stamp := time.Now().Format("20060102-150405")
	targetName := fmt.Sprintf("pr-%d", pr.Number)
	if tgt.BranchOnly {
		targetName = "branch-" + safeRunPart(r.branch)
	}
	r.root = filepath.Join(r.o.RunsDir, fmt.Sprintf("%s-%s-%s",
		strings.ReplaceAll(r.o.Repo, "/", "-"), targetName, stamp))
	r.worktree = filepath.Join(r.root, "worktree")
	r.logDir = filepath.Join(r.root, "logs")
	r.msgDir = filepath.Join(r.root, "messages")
	// Cache collection takes this same lock from its liveness check through
	// dependency eviction. Create and claim the run while holding it so
	// collection must happen wholly before this startup or see this run as live.
	unlockCache, err := proc.LockDir(filepath.Dir(r.o.DepsDir))
	if err != nil {
		return err
	}
	for _, d := range []string{r.logDir, r.msgDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			unlockCache()
			return err
		}
	}
	r.releaseClaim, err = proc.Claim(r.root)
	if err != nil {
		unlockCache()
		return err
	}
	r.gcOldRuns()
	unlockCache()

	// Published before the worktree and its dependencies are prepared, which on
	// a cold cache is minutes: a run has to appear on the dashboard when it
	// starts, not when it gets around to reviewing something.
	r.prog = Progress{
		PID:        os.Getpid(),
		Repo:       r.o.Repo,
		Number:     pr.Number,
		Title:      pr.Title,
		Branch:     r.branch,
		StartedAt:  time.Now(),
		MaxIter:    r.o.MaxIter,
		MaxCIFixes: r.o.MaxCIFixes,
	}
	r.enter(PhaseStarting)

	if err := r.p.Git.WorktreeAdd(r.ctx, r.o.RepoRoot, r.worktree, headSHA); err != nil {
		return err
	}

	r.env = envexec.Env{Worktree: r.worktree, Direnv: r.o.UseDirenv, DirenvBin: r.o.DirenvBin}
	r.codex = codex.Options{
		Bin: r.o.CodexBin, Model: r.o.Model, Effort: r.o.Effort, Bypass: r.o.Bypass,
	}
	r.rules = standingRules(r.branch, !r.o.Interactive, tgt.BranchOnly)
	if tgt.BranchOnly {
		r.prCtx = branchContext(r.branch, pr.BaseRefName, r.o.Context)
	} else {
		r.prCtx = prContext(pr.Number, pr.Title, r.branch, pr.BaseRefName, pr.URL, pr.Body, r.o.Context)
	}

	if r.o.UseDirenv {
		if err := r.setupDirenv(); err != nil {
			return err
		}
	}

	r.rep.Header(Header{
		Repo: r.o.Repo, Number: pr.Number, Title: pr.Title,
		Branch: r.branch, Base: pr.BaseRefName, BranchOnly: tgt.BranchOnly,
		Model: r.o.Model, Effort: r.o.Effort, Bypass: r.o.Bypass,
		Interactive: r.o.Interactive, MaxIter: r.o.MaxIter, MaxCIFixes: r.o.MaxCIFixes,
		FixTimeout: r.o.FixTimeout, RunDir: r.root, Worktree: r.worktree, HeadSHA: headSHA,
	})
	if tgt.BranchOnly {
		r.rep.Warn("no open PR: GitHub PR checks and PR comments are skipped; fix steps still run repository tests")
	}
	return nil
}

func safeRunPart(s string) string {
	s = strings.ReplaceAll(s, "/", "-")
	s = strings.ReplaceAll(s, "\\", "-")
	s = strings.ReplaceAll(s, "..", "-")
	if s == "" {
		return "unknown"
	}
	return s
}

// execute is the main loop.
func (r *run) execute() (*Result, error) {
	res := &Result{PR: r.pr, BranchOnly: r.target.BranchOnly, RunDir: r.root}

	r.startReview(1)
	if !r.target.BranchOnly {
		r.rep.Step("CI")
		r.enter(PhaseCI)
		// The first review only needs the pushed head, so it starts now and runs
		// while the initial CI wait happens.
		if err := r.ensureCIGreen(); err != nil {
			r.killReview()
			return res, err
		}
	}

	for iteration := 1; iteration <= r.o.MaxIter; iteration++ {
		res.Rounds = iteration
		r.rep.Step(fmt.Sprintf("Review round %d/%d", iteration, r.o.MaxIter))
		r.prog.Round = iteration
		r.enter(PhaseReview)

		findings, comment, err := r.finishReview(iteration)
		if err != nil {
			return res, err
		}
		res.LastFindings = findings

		currentSHA, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
		if err != nil {
			return res, err
		}
		// The findings describe one specific head. If the branch moved since,
		// acting on them would fix the wrong code.
		if findings.HeadSHA != currentSHA {
			return res, fmt.Errorf("the review is for %s but the branch is at %s", findings.HeadSHA, currentSHA)
		}

		r.rep.RoundResult(iteration, findings, findings.Blocking() == 0)
		r.prog.Reviewed = true
		r.prog.Blockers = findings.Blockers
		r.prog.Critical = findings.Critical
		r.publish()
		if findings.Blocking() == 0 {
			res.Converged = true
			break
		}

		r.rep.Step(fmt.Sprintf("Fix round %d/%d", iteration, r.o.MaxIter))
		r.enter(PhaseFix)
		preFixSHA := currentSHA
		tag := fmt.Sprintf("fix-round-%d", iteration)

		if err := r.codexCall(tag, fixRoundPrompt(r.pr.Number, r.branch, r.target.BranchOnly, comment)); err != nil {
			return res, err
		}
		if err := r.questionGate(tag); err != nil {
			return res, err
		}
		if err := r.ensureCommitted(tag); err != nil {
			return res, err
		}

		afterSHA, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
		if err != nil {
			return res, err
		}
		if afterSHA == preFixSHA {
			if !hasMarker(r.lastMsg, MarkerDisputed) {
				r.rep.Notify("Festgefahren", fmt.Sprintf("Fix-Runde %d hat nichts geaendert", iteration))
				return res, fmt.Errorf("%w: round %d, a human is needed: %s",
					ErrNoProgress, iteration, r.targetReference())
			}
			if err := r.disputeGate(tag, preFixSHA); err != nil {
				return res, err
			}
			if r.disputeAccepted {
				res.Converged = true
				res.DisputeAccepted = true
				res.DisputeText = r.disputeText
				break
			}
			// The dispute gate only returns unaccepted once new commits exist.
		}

		r.recordRound(fmt.Sprintf("Review fix round %d", iteration), preFixSHA)
		if err := r.pushBranch(); err != nil {
			return res, err
		}
		r.postFixComment(iteration, preFixSHA)

		// Overlap the next review with this round's CI wait.
		if iteration < r.o.MaxIter {
			r.startReview(iteration + 1)
		}
		if !r.target.BranchOnly {
			if err := r.ensureCIGreen(); err != nil {
				r.killReview()
				return res, err
			}
		}
	}

	res.RoundLog = r.roundLog
	if !res.Converged {
		r.rep.Notify("Nicht konvergiert", fmt.Sprintf("%s hat nach %d Runden weiter Findings", r.targetLabel(), r.o.MaxIter))
		return res, fmt.Errorf("%w after %d review rounds", ErrNotConverged, r.o.MaxIter)
	}
	return res, nil
}

func (r *run) targetLabel() string {
	if r.target.BranchOnly {
		return "Branch " + r.branch
	}
	return fmt.Sprintf("PR #%d", r.pr.Number)
}

func (r *run) targetReference() string {
	if r.target.BranchOnly || r.pr.URL == "" {
		return r.branch
	}
	return r.pr.URL
}

// codexCall runs a step in the shared session, starting it on the first call.
func (r *run) codexCall(tag, prompt string) error {
	if err := r.checkEnvrc(); err != nil {
		return err
	}
	msgPath := filepath.Join(r.msgDir, tag+".md")
	logPath := filepath.Join(r.logDir, tag+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		return err
	}
	defer logFile.Close()

	// The log always gets everything; --verbose additionally mirrors it to the
	// terminal, which is the only way to watch a fix session work.
	var out io.Writer = logFile
	if r.o.Verbose && r.o.Out != nil {
		out = io.MultiWriter(logFile, r.o.Out)
	}

	r.rep.StepStart(stepLabel(tag))
	started := time.Now()

	if r.sessionID == "" {
		err = r.codex.Exec(r.ctx, r.env, r.o.FixTimeout,
			firstPrompt(r.rules, r.prCtx, prompt), msgPath, out)
	} else {
		err = r.codex.Resume(r.ctx, r.env, r.o.FixTimeout, r.sessionID, prompt, msgPath, out)
	}
	r.rep.StepEnd(stepLabel(tag), time.Since(started), err == nil)

	if err != nil {
		if errors.Is(err, proc.ErrTimeout) {
			return fmt.Errorf("codex fix step timed out after %s (--fix-timeout; 0 disables), see %s",
				r.o.FixTimeout, logPath)
		}
		return fmt.Errorf("codex exec failed with exit %d, see %s", proc.ExitCode(err), logPath)
	}

	data, err := os.ReadFile(msgPath)
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return fmt.Errorf("codex produced no final message, see %s", logPath)
	}
	r.lastMsg = string(data)

	if r.sessionID == "" {
		id, err := codex.FindSession(r.worktree)
		if err != nil {
			return fmt.Errorf("could not find the Codex session for %s: %w", r.worktree, err)
		}
		r.sessionID = id
	}
	return nil
}

// resume is codexCall for steps that must not start a new session.
func (r *run) resume(tag, prompt string) error {
	if r.sessionID == "" {
		return fmt.Errorf("no Codex session to resume")
	}
	return r.codexCall(tag, prompt)
}

// ensureCommitted makes sure the worktree is clean before the pipeline reads
// the head sha to decide whether a round changed anything.
func (r *run) ensureCommitted(tag string) error {
	dirty, err := r.p.Git.Dirty(r.ctx, r.worktree)
	if err != nil {
		return err
	}
	if !dirty {
		return nil
	}
	if err := r.codexCall(tag+"-commit", commitPrompt); err != nil {
		return err
	}
	if err := r.questionGate(tag + "-commit"); err != nil {
		return err
	}
	dirty, err = r.p.Git.Dirty(r.ctx, r.worktree)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("worktree still has uncommitted changes; inspect %s", r.worktree)
	}
	return nil
}

// pushBranch is the safety net for a push the session forgot, and the barrier
// that keeps a stale check result from passing as green.
//
// After a push, `gh pr checks` can briefly still report the previous head's
// completed checks. A red new commit would then read as green, so nothing is
// trusted until GitHub reports the pushed sha as the PR head.
func (r *run) pushBranch() error {
	pushedSHA, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
	if err != nil {
		return err
	}
	// Pre-push hooks spam the terminal and can fail spuriously when they race a
	// push the session already made, so their output only surfaces when the
	// push failed and the sha never arrived.
	out, pushErr := r.p.Git.Push(r.ctx, r.worktree, "origin", r.branch)
	logPath := filepath.Join(r.logDir, "push-last.log")
	os.WriteFile(logPath, []byte(out), 0o644)

	deadline := time.Now().Add(headSettleTimeout)
	for {
		var remote string
		if r.target.BranchOnly {
			remote, _ = r.p.Git.LsRemote(r.ctx, r.o.RepoRoot, "origin", "refs/heads/"+r.branch)
		} else {
			remote, _ = r.p.GH.HeadSHA(r.ctx, r.o.RepoRoot, r.pr.Number)
		}
		if remote == pushedSHA {
			return nil
		}
		if pushErr != nil {
			return fmt.Errorf("git push failed and origin/%s does not have %s:\n%s", r.branch, pushedSHA, out)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("origin/%s still reports %s instead of the pushed %s after %s; is someone else pushing to it?",
				r.branch, remote, pushedSHA, headSettleTimeout)
		}
		r.rep.Info("waiting for GitHub to register the pushed head...")
		select {
		case <-time.After(5 * time.Second):
		case <-r.ctx.Done():
			return r.ctx.Err()
		}
	}
}

// recordRound stores a round's commits for the closing summary.
func (r *run) recordRound(label, preSHA string) {
	commits := r.p.Git.LogOneline(r.ctx, r.worktree, preSHA+"..HEAD")
	if commits == "" {
		return
	}
	r.roundLog = append(r.roundLog, RoundEntry{Label: label, Commits: commits})
	r.prog.Commits += len(splitLines(commits))
	r.publish()
	f, err := os.OpenFile(filepath.Join(r.logDir, "rounds.log"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		fmt.Fprintf(f, "%s:\n%s\n\n", label, commits)
		f.Close()
	}
}

// postFixComment posts the round's fix log to the PR.
//
// The text comes from the session's PR COMMENT block. A later gate step may
// have produced the newest message, so the round's own message is checked as
// well; the commit list is the fallback. The pipeline posts it rather than the
// session, which is what keeps it a normal comment from the user.
func (r *run) postFixComment(round int, preSHA string) {
	if r.target.BranchOnly {
		return
	}
	body := section(r.lastMsg, MarkerComment)
	if body == "" {
		if data, err := os.ReadFile(filepath.Join(r.msgDir, fmt.Sprintf("fix-round-%d.md", round))); err == nil {
			body = section(string(data), MarkerComment)
		}
	}
	if body != "" {
		// Drop the marker line itself.
		if _, rest, ok := strings.Cut(body, "\n"); ok {
			body = rest
		} else {
			body = ""
		}
	}
	if strings.TrimSpace(body) == "" {
		commits := r.p.Git.LogOneline(r.ctx, r.worktree, preSHA+"..HEAD")
		if commits == "" {
			return
		}
		body = fmt.Sprintf("Review round %d:\n\n```\n%s\n```", round, commits)
	}
	if _, err := r.p.GH.CommentBody(r.ctx, r.o.RepoRoot, r.pr.Number, body); err != nil {
		r.rep.Warn(fmt.Sprintf("could not post the fix-log comment to PR #%d: %v", r.pr.Number, err))
	}
}

// gcOldRuns drops run directories nothing has looked at for a week.
func (r *run) gcOldRuns() {
	entries, err := os.ReadDir(r.o.RunsDir)
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-runRetention)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(r.o.RunsDir, e.Name())
		if proc.Claimed(dir) {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			continue
		}
		r.p.Git.WorktreeRemove(r.ctx, r.o.RepoRoot, filepath.Join(dir, "worktree"))
		os.RemoveAll(dir)
	}
	r.p.Git.WorktreePrune(r.ctx, r.o.RepoRoot)
}

// cleanup stops a background review and removes the worktree on success.
func (r *run) cleanup() {
	r.killReview()
	if !r.o.KeepWorktree {
		r.p.Git.WorktreeRemove(r.ctx, r.o.RepoRoot, r.worktree)
	}
	r.releaseRunClaim()
}

func (r *run) releaseRunClaim() {
	if r.releaseClaim != nil {
		r.releaseClaim()
		r.releaseClaim = nil
	}
}

func (o Options) withDefaults() Options {
	if o.MaxIter == 0 {
		o.MaxIter = DefaultMaxIter
	}
	if o.MaxCIFixes == 0 {
		o.MaxCIFixes = DefaultMaxCIFixes
	}
	if o.FixTimeout == 0 {
		o.FixTimeout = DefaultFixTimeout
	}
	return o
}

func (o Options) validate() error {
	if o.MaxIter < 1 {
		return fmt.Errorf("max-iter must be >= 1")
	}
	if !codex.ValidEffort(o.Effort) {
		return fmt.Errorf("effort must be one of: %s", strings.Join(codex.Efforts, ", "))
	}
	if !codex.ValidEffort(o.ReviewEffort) {
		return fmt.Errorf("review-effort must be one of: %s", strings.Join(codex.Efforts, ", "))
	}
	if o.Repo == "" || o.RepoRoot == "" {
		return fmt.Errorf("repo and repo root are required")
	}
	return nil
}

func (p *Pipeline) reporter() Reporter {
	if p.Rep == nil {
		return NopReporter{}
	}
	return p.Rep
}

// stepLabel turns an internal tag into what the status line shows.
func stepLabel(tag string) string {
	switch {
	case strings.HasPrefix(tag, "fix-round-"):
		return "Fix round " + strings.TrimPrefix(tag, "fix-round-")
	case strings.HasPrefix(tag, "ci-fix-"):
		return "CI fix " + strings.TrimPrefix(tag, "ci-fix-")
	default:
		return "Codex " + tag
	}
}

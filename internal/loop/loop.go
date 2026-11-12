// Package loop drives a pull request from "implementation pushed" to "ready
// for manual testing": wait for CI, review, feed the findings into a resumable
// fix session that fixes them, push, repeat until a review comes back with no
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
	"unicode"

	"github.com/yungweng/quorum/internal/engine"
	"github.com/yungweng/quorum/internal/engine/spec"
	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/proc"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/rundir"
	"github.com/yungweng/quorum/internal/runtmp"
	"github.com/yungweng/quorum/internal/target"
	"github.com/yungweng/quorum/internal/usagelimit"
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
	// ErrNoProgress is exit 5: a fix step changed nothing while its finding or
	// failing verification remains.
	ErrNoProgress = errors.New("fix step made no progress while its failure remains")
	// ErrDiverged is exit 6: the bounded analysis found incompatible review/fix
	// decisions and produced a report for manual resolution.
	ErrDiverged = errors.New("review loop diverged")
	// ErrConflicts is exit 7: the branch conflicts with its base and the
	// resolution session did not produce a conflict-free merge.
	ErrConflicts = errors.New("merge conflicts with the base branch remain")
	// ErrTestsRed is the offline loop's local counterpart of ErrCIRed and maps
	// onto the same exit code: the configured test command stayed red after the
	// allowed fix attempts.
	ErrTestsRed = errors.New("the test command still fails after the allowed fix attempts")
	// ErrCIUnconfirmed maps onto exit 3 as well: GitHub never confirmed CI for
	// the head - a required check did not report, or a workflow run failed
	// without a check a fix session could be pointed at. The run stops rather
	// than reading as green or spending a fix session on it.
	ErrCIUnconfirmed = errors.New("CI not confirmed green for the head")
)

// Defaults for a run.
const (
	defaultMaxIter    = 12
	defaultMaxCIFixes = 3
	defaultFixTimeout = 2 * time.Hour
	// maxQuestionBounces is how often autonomous mode sends questions back
	// before giving up rather than looping forever.
	maxQuestionBounces = 3
	// headSettleTimeout is how long GitHub gets to report a pushed sha as the
	// PR head before the pipeline treats it as a problem.
	headSettleTimeout = 180 * time.Second
	// A golangci-lint process uses a shared cache lock and rejects concurrent
	// runners. The hook itself can be expensive, so a small bounded retry is
	// enough to outlive the competing run without handing a transient lock to a
	// fix session as though it were a code defect.
	prePushLockAttempts   = 3
	prePushLockRetryDelay = 5 * time.Second
)

// timings are the waits of one run. The zero value is production's: every
// field left zero falls back to its default, and tests shorten them per run.
type timings struct {
	// progress is how often blocking work reports its elapsed time.
	progress time.Duration
	// ciPendingPoll and ciNoChecksPoll space the CI wait's polls while checks
	// are pending, and while GitHub reports none at all.
	ciPendingPoll  time.Duration
	ciNoChecksPoll time.Duration
	// noChecksTimeout is how long GitHub gets to register the checks and
	// Actions runs of a head before the wait accepts that there are none, or
	// stops on a required check that never reported.
	noChecksTimeout time.Duration
	// prePushRetry is the pause before pre-push verification is retried after
	// losing the golangci-lint lock.
	prePushRetry time.Duration
}

func orDefault(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}

func (t timings) progressTick() time.Duration { return orDefault(t.progress, time.Second) }
func (t timings) ciPending() time.Duration    { return orDefault(t.ciPendingPoll, 10*time.Second) }
func (t timings) ciNoChecks() time.Duration   { return orDefault(t.ciNoChecksPoll, 20*time.Second) }
func (t timings) noChecksWindow() time.Duration {
	return orDefault(t.noChecksTimeout, 180*time.Second)
}
func (t timings) prePushRetryDelay() time.Duration {
	return orDefault(t.prePushRetry, prePushLockRetryDelay)
}

type Options struct {
	Repo     string
	RepoRoot string
	Number   int
	Context  string // extra text handed to the fix session
	// Rules is the repository's user-local review rules file, verbatim. Review
	// rounds hand it to every reviewer pass, the aggregator and the verifier;
	// fix sessions get it appended to their standing rules so a fix cannot
	// resolve a finding by violating the rule that produced it.
	Rules string

	Engine string // fix-session engine: codex, claude or grok; empty means codex
	Model  string
	Effort string

	// Review is the template every review round starts from. It carries the
	// rounds' own settings: panel size, engine, model, effort, timeout, run
	// root and concurrency. Everything a round shares with the loop or that
	// changes per round (repository, checkout, target, head, base, rules,
	// posting, direnv, the dependency cache, binaries, resume and worktree
	// retention) is set from the loop's own state and overrides whatever the
	// template holds.
	Review review.Options

	MaxIter    int
	MaxCIFixes int
	FixTimeout time.Duration

	DivergenceScan       bool
	DivergenceEscalateTo []string
	DivergenceTimeout    time.Duration
	// Post controls every pull request write produced by the run and enables
	// final-description candidate generation after convergence.
	Post bool

	// AllowDraft permits the run to work on a draft pull request. Drafts are
	// refused otherwise: pushing fix commits and posting comments to a PR its
	// author marked "not ready" needs an explicit go-ahead.
	AllowDraft bool
	// Local ignores any open pull request and runs branch-only on the current
	// pushed branch: no PR comments, no PR CI wait, no auto-merge.
	Local bool
	// Offline keeps every review-fix iteration local: reviews run against the
	// worktree's unpushed head, fix rounds commit without pushing, and the
	// configured TestCmd replaces the per-round CI wait. Only a converged run
	// pushes - once - and triggers a single CI run. This is what keeps a
	// babysit run from billing one GitHub Actions run per fix round.
	Offline bool
	// TestCmd is the repository's local test command, run through the shell in
	// the worktree after every offline round that committed something. Empty
	// disables the deterministic gate; the session prompts still demand that
	// affected checks are run.
	TestCmd    string
	TestCmdSet bool // true when an explicit or user-local empty command disables fallback
	// ResolveConflicts keeps the branch current with origin/<base>. Clean
	// updates use git directly; conflicts go through the fix session. Any
	// update invalidates an older review of the branch head.
	ResolveConflicts bool
	// BaseUpdateOnConflict narrows ResolveConflicts to bases that would
	// conflict; a branch that is merely behind is reviewed as it is.
	BaseUpdateOnConflict bool
	// FixSuggestions runs one terminal fix round when the final review is
	// clean but still lists Suggestions: triage each one, implement what is
	// worth it, and end the run without another review.
	FixSuggestions bool
	// ResumeRun reuses a prior review run's completed reviewer output for the
	// first review round, the way a failed round's in-run retry does. Later
	// rounds always fan out fresh: they review a head the saved output has
	// never seen.
	ResumeRun string

	// Bypass runs the fix sessions with the engine's sandbox and approval
	// bypass. They must run tests, use gh and push, unattended, and a
	// sandboxed exec would silently skip exactly those commands.
	Bypass      bool
	Interactive bool
	UseDirenv   bool
	// AllowEnvrcChange permits direnv to load an .envrc changed by the target.
	AllowEnvrcChange bool
	KeepWorktree     bool

	// Verbose streams the full fix-session output instead of only the status
	// line.
	// The logs are written either way; this decides what reaches the terminal.
	Verbose bool
	// Out is where verbose output goes. Nil discards it.
	Out io.Writer

	RunsDir   string
	DepsDir   string
	Bins      spec.Bins
	DirenvBin string
}

// RoundEntry is one round's commit list, for the closing summary.
type RoundEntry struct {
	Label   string
	Commits string
}

type Result struct {
	PR         gh.FullPR
	BranchOnly bool
	// Local records that the run deliberately ignored any open PR, so the
	// summary can say "local run" instead of claiming there is no PR.
	Local           bool
	Rounds          int
	Converged       bool
	DisputeAccepted bool
	DisputeText     string
	// DisputeCommentURL is the linked rebuttal posted after an accepted dispute.
	// DisputeCommentPosted distinguishes a successful post with no URL in gh's
	// output from a failed post.
	DisputeCommentURL    string
	DisputeCommentPosted bool
	RoundLog             []RoundEntry
	RunDir               string
	Duration             time.Duration
	// SuggestionCommits records that the terminal suggestion round pushed
	// commits the final review has never seen. Auto-merge must skip such a
	// head: the approval would claim a review that did not happen.
	SuggestionCommits    bool
	LastFindings         review.Findings
	Divergence           *DivergenceReport
	DivergenceReportPath string
	DivergenceCommentURL string
	PRDescriptionFile    string
	PRDescriptionCurrent bool
	// PRDescriptionUpdated means the candidate replaced the remote body after
	// the final drift check passed.
	PRDescriptionUpdated bool
}

type Pipeline struct {
	GH     *gh.Client
	Git    git.G
	Review Reviewer
	Rep    Reporter
	// In supplies answers at interactive gates. Nil means no terminal, which
	// makes every interactive gate abort rather than hang.
	In io.Reader
}

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
	succeeded := false
	defer func() { r.cleanup(succeeded) }()

	res, err := r.execute()
	succeeded = err == nil
	if res != nil {
		res.Duration = time.Since(started)
	}
	return res, err
}

type run struct {
	p   *Pipeline
	o   Options
	ctx context.Context
	rep Reporter

	target       target.Target
	pr           gh.FullPR
	branch       string
	headSHA      string
	root         string
	worktree     string
	hooksPath    string
	logDir       string
	msgDir       string
	releaseClaim func()
	removeTmp    func()

	env   envexec.Env
	fixer engine.Fixer
	rules string
	prCtx string

	// The two models this run works with, resolved once in prepare. Every step
	// reports one of them, and the review side is also what the divergence scan
	// and the final PR description run on.
	reviewModel spec.Model
	fixModel    spec.Model

	// prompt is what every fix prompt says about the target, fixed in prepare.
	prompt promptTarget
	timing timings

	// The fix session, shared across every CI fix and review round, so context
	// carries through the whole run. lastMsg is its latest final message.
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
	testFixTotal    int
	pushFixTotal    int
	testRuns        int
	conflictFixes   int
	divergenceTrace divergenceTrace

	// suggestionDone keeps the terminal suggestion round terminal in offline
	// mode, where a CI repair after the push can send the run through another
	// review round: the suggestion triage still runs at most once per run.
	suggestionDone bool
	// pendingFixComments are the offline rounds' PR COMMENT blocks, held back
	// until the final push makes the commits they describe public.
	pendingFixComments []string

	// prog is what this run publishes about itself for a watcher to read. It
	// is the run's own copy; publish writes it out.
	prog Progress
}

func (r *run) prepare() error {
	var tgt target.Target
	var err error
	if r.o.Local {
		tgt, err = target.ResolveLocal(r.ctx, r.p.GH, r.p.Git, r.o.RepoRoot)
	} else {
		tgt, err = target.Resolve(r.ctx, r.p.GH, r.p.Git, r.o.RepoRoot, r.o.Number, "", "")
	}
	if err != nil {
		return err
	}
	pr := tgt.PR
	if !tgt.BranchOnly && pr.State != gh.StateOpen {
		return fmt.Errorf("PR is %s, expected an open PR", pr.State)
	}
	if err := refuseDraft(pr, tgt.BranchOnly, r.o.AllowDraft); err != nil {
		return err
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

	if err := r.p.Git.FetchBranch(r.ctx, r.o.RepoRoot, r.branch); err != nil {
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
	r.headSHA = headSHA
	if r.p.Git.HasLocalBranch(r.ctx, r.o.RepoRoot, r.branch) {
		localSHA, err := r.p.Git.RevParse(r.ctx, r.o.RepoRoot, "refs/heads/"+r.branch)
		if err != nil {
			return err
		}
		if localSHA != headSHA {
			// The pipeline's own pushes never move the local ref, so after any
			// earlier run the local branch is simply behind origin. That loses
			// nothing: the run works on origin's head in its own worktree.
			// Only a local branch with commits of its own is refused.
			behind, aerr := r.p.Git.IsAncestor(r.ctx, r.o.RepoRoot, localSHA, headSHA)
			if aerr != nil {
				return aerr
			}
			if !behind {
				return fmt.Errorf("local branch %s (%s) differs from origin (%s); push your local commits first",
					r.branch, localSHA, headSHA)
			}
			r.rep.Warn(fmt.Sprintf("local branch %s is behind origin; the run works on origin's head %s",
				r.branch, shortSHA(headSHA)))
		}
	}

	number := pr.Number
	if tgt.BranchOnly {
		number = 0
	}
	r.root = rundir.New(r.o.RunsDir, r.o.Repo, number, r.branch)
	r.worktree = rundir.Worktree(r.root)
	r.logDir = filepath.Join(r.root, logsSubdir)
	r.msgDir = filepath.Join(r.root, messagesSubdir)
	r.releaseClaim, err = rundir.Start(r.o.DepsDir, r.root, []string{logsSubdir, messagesSubdir}, func() {
		rundir.Sweep(r.ctx, r.p.Git, r.o.RepoRoot, r.o.RunsDir, r.root)
	})
	if err != nil {
		return err
	}
	runtmp.Sweep()

	// The review side is reported resolved, not as configured: an empty review
	// model becomes the engine's default here in quorum, so passing the raw
	// value on would announce "engine default" for a run that is about to use a
	// model this code already picked. The fix side stays as configured, because
	// an empty one really is the CLI's own choice and quorum never learns it.
	reviewEngine, reviewModel, reviewEffort := spec.ReviewDefaults(
		r.o.Review.Engine, r.o.Review.Model, r.o.Review.Effort)
	r.reviewModel = spec.Model{Engine: reviewEngine, Name: reviewModel, Effort: reviewEffort}
	r.fixModel = spec.Model{Engine: r.o.Engine, Name: r.o.Model, Effort: r.o.Effort}

	// Published before the worktree and its dependencies are prepared, which on
	// a cold cache is minutes: a run has to appear on the dashboard when it
	// starts, not when it gets around to reviewing something.
	r.prog = Progress{
		PID:        os.Getpid(),
		Repo:       r.o.Repo,
		Number:     pr.Number,
		Title:      pr.Title,
		Author:     pr.Author.Login,
		Branch:     r.branch,
		StartedAt:  time.Now(),
		MaxIter:    r.o.MaxIter,
		MaxCIFixes: r.o.MaxCIFixes,
		Review:     r.reviewModel,
		Fix:        r.fixModel,
	}
	r.enter(PhaseStarting)

	if err := r.p.Git.WorktreeAdd(r.ctx, r.o.RepoRoot, r.worktree, headSHA); err != nil {
		return err
	}
	tmpDir, removeTmp, err := runtmp.New()
	if err != nil {
		return fmt.Errorf("create private TMPDIR: %w", err)
	}
	r.removeTmp = removeTmp
	r.env, err = rundir.Isolate(r.ctx, r.p.Git, r.worktree, tmpDir, r.o.UseDirenv, r.o.DirenvBin)
	if err != nil {
		return err
	}
	r.hooksPath = r.env.GitHooksPath
	fixer, err := engine.NewFixer(r.fixModel, r.o.Bins, r.o.Bypass)
	if err != nil {
		return err
	}
	r.fixer = fixer
	r.prompt = promptTarget{number: pr.Number, branch: r.branch, branchOnly: tgt.BranchOnly, offline: r.o.Offline}
	r.rules = standingRules(r.prompt, !r.o.Interactive, r.o.Rules)
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

	// The repository's own gate is the fallback: an explicit --test-cmd or the
	// user-local per-repo file stay personal overrides.
	testCmdNote := ""
	if r.o.Offline && !r.o.TestCmdSet && r.o.TestCmd == "" {
		cmd, err := resolveRepoTestCmd(r.ctx, r.p.Git, r.o.RepoRoot, pr.BaseRefName)
		if err != nil {
			return err
		}
		if cmd != "" {
			r.o.TestCmd = cmd
			testCmdNote = fmt.Sprintf("%s @ %s", RepoTestCmdPath, pr.BaseRefName)
		}
	}

	r.rep.Header(Header{
		Repo: r.o.Repo, Number: pr.Number, Title: pr.Title, URL: pr.URL,
		Branch: r.branch, Base: pr.BaseRefName, BranchOnly: tgt.BranchOnly,
		Draft: pr.IsDraft, Local: r.o.Local,
		Fix: r.fixModel, Bypass: r.o.Bypass,
		Review:      spec.Model{Engine: r.o.Review.Engine, Name: reviewModel, Effort: reviewEffort},
		Interactive: r.o.Interactive, Offline: r.o.Offline, TestCmd: r.o.TestCmd, TestCmdNote: testCmdNote,
		MaxIter: r.o.MaxIter, MaxCIFixes: r.o.MaxCIFixes,
		DivergenceScan: r.o.DivergenceScan,
		FixTimeout:     r.o.FixTimeout, RunDir: r.root, HeadSHA: headSHA,
	})
	switch {
	case r.o.Local:
		r.rep.Warn("local run: any open PR is left alone; GitHub PR checks and PR comments are skipped; fix steps still run repository tests")
	case tgt.BranchOnly:
		r.rep.Warn("no open PR: GitHub PR checks and PR comments are skipped; fix steps still run repository tests")
	}
	return nil
}

// refuseDraft is the draft gate: a draft PR is one its author marked "not
// ready", so pushing fixes and posting comments to it needs an explicit
// go-ahead via --draft or BABYSIT_DRAFTS=1.
func refuseDraft(pr gh.FullPR, branchOnly, allow bool) error {
	if branchOnly || !pr.IsDraft || allow {
		return nil
	}
	return fmt.Errorf("PR #%d is a draft; rerun with --draft, or set BABYSIT_DRAFTS=1 to always allow drafts", pr.Number)
}

func (r *run) execute() (*Result, error) {
	res := &Result{PR: r.pr, BranchOnly: r.target.BranchOnly, Local: r.o.Local, RunDir: r.root}
	if r.o.DivergenceScan {
		r.startDivergenceTrace()
	}

	// Update from the base before anything else so the first review covers the
	// code that will land rather than an already stale branch head.
	baseUpdated, err := r.ensureBaseCurrent()
	if err != nil {
		return res, err
	}

	// A base update above moved the head, so a handed-in resume would replay
	// reviewer output for a head that no longer exists; only an untouched head
	// may reuse it.
	resume := r.o.ResumeRun
	if baseUpdated {
		resume = ""
	}
	r.startReviewWith(1, resume)
	// Offline runs skip the initial CI wait: the local test gate guards each
	// round and the single CI run after the final push guards the result.
	if !r.target.BranchOnly && !r.o.Offline {
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
		r.rep.Step(fmt.Sprintf("Round %d/%d", iteration, r.o.MaxIter))
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
		if r.o.DivergenceScan {
			r.traceReview(iteration, findings, comment)
		}

		r.rep.RoundResult(iteration, findings, findings.Blocking() == 0)
		r.prog.Reviewed = true
		r.prog.Blockers = findings.Blockers
		r.prog.Critical = findings.Critical
		r.publish()
		if findings.Blocking() == 0 {
			if r.o.Offline {
				settled, err := r.finalizeOffline(res, iteration, findings, comment)
				if err != nil {
					return res, err
				}
				if settled {
					res.Converged = true
					break
				}
				// CI repairs moved the head past the reviewed commit, so the
				// clean review no longer describes what is on the branch. The
				// next round reviews the repaired head.
				r.reviewNext(iteration, "")
				continue
			}
			baseUpdated, err := r.ensureBaseCurrent()
			if err != nil {
				return res, err
			}
			if baseUpdated {
				if err := r.prepareReviewAfterBaseUpdate(iteration, "clean"); err != nil {
					return res, err
				}
				continue
			}
			res.Converged = true
			if suggestionRoundDue(r.o, findings) {
				pushed, err := r.suggestionRound(iteration, findings, comment, currentSHA)
				res.SuggestionCommits = pushed
				if err != nil {
					return res, err
				}
			}
			break
		}

		r.enter(PhaseFix)
		preFixSHA := currentSHA
		step, err := r.fixStep(fmt.Sprintf("fix-round-%d", iteration), fixRoundPrompt(r.prompt, comment))
		if err != nil {
			return res, err
		}
		afterSHA := step.head
		if afterSHA == preFixSHA {
			if !hasMarker(step.last, markerDisputed) {
				r.rep.Notify("Festgefahren", fmt.Sprintf("Fix-Runde %d hat nichts geaendert", iteration))
				return res, fmt.Errorf("%w: round %d, a human is needed: %s",
					ErrNoProgress, iteration, r.targetReference())
			}
			dispute, err := r.disputeGate(step, preFixSHA)
			if err != nil {
				return res, err
			}
			if dispute.accepted {
				converged, err := r.acceptDispute(res, iteration, findings, step, preFixSHA, dispute.text)
				if err != nil {
					return res, err
				}
				if converged {
					break
				}
				continue
			}
			// The dispute gate only returns unaccepted once new commits exist.
			// The re-check that made them describes them.
			afterSHA, step.last = dispute.head, dispute.last
		}
		if r.o.DivergenceScan {
			r.traceFix(iteration, preFixSHA, afterSHA, step.first)
		}

		r.recordRound(fmt.Sprintf("Review fix round %d", iteration), preFixSHA)
		if r.o.Offline {
			// The commits stay local: the comment is queued for the final
			// push, and the test gate replaces this round's CI wait.
			r.queueFixComment(step, fmt.Sprintf("Review fix round %d", iteration), preFixSHA)
		} else {
			if err := r.pushBranchWithFixes(); err != nil {
				return res, err
			}
			if err := r.postFixComment(step, fmt.Sprintf("Review fix round %d", iteration),
				fmt.Sprintf("Review round %d", iteration), findingsCommentURL(findings), preFixSHA); err != nil {
				return res, err
			}
		}

		// The base may have moved during the round. Re-checking here, while no
		// review is running, keeps the next review on an up-to-date head.
		if _, err := r.ensureBaseCurrent(); err != nil {
			return res, err
		}
		if r.o.Offline {
			if err := r.ensureTestsGreen(); err != nil {
				return res, err
			}
			// Test repairs can move the local head after the first check above.
			// Refresh the base before starting a review of those new commits.
			if _, err := r.ensureBaseCurrent(); err != nil {
				return res, err
			}
		}

		// Overlap the next review with this round's CI wait.
		r.reviewNext(iteration, "")
		if !r.target.BranchOnly && !r.o.Offline {
			if err := r.ensureCIGreen(); err != nil {
				r.killReview()
				return res, err
			}
		}
	}

	res.RoundLog = r.roundLog
	if !res.Converged {
		if r.o.DivergenceScan {
			if err := r.runDivergenceScan(res); err != nil {
				return res, fmt.Errorf("%w after %d review rounds; divergence scan failed: %v",
					ErrNotConverged, r.o.MaxIter, err)
			}
			if res.Divergence != nil && res.Divergence.Verdict == DivergenceDiverged {
				return res, fmt.Errorf("%w after %d review rounds", ErrDiverged, r.o.MaxIter)
			}
		}
		r.rep.Notify("Nicht konvergiert", fmt.Sprintf("%s hat nach %d Runden weiter Findings", r.targetLabel(), r.o.MaxIter))
		return res, fmt.Errorf("%w after %d review rounds", ErrNotConverged, r.o.MaxIter)
	}
	if err := r.finishPRDescription(res); err != nil {
		return res, err
	}
	return res, nil
}

// acceptDispute finishes a round whose rebuttal survived the dispute gate. It
// reports whether the run converged on it; false means the head moved past the
// disputed review and the next round's review has been started.
func (r *run) acceptDispute(res *Result, round int, findings review.Findings, step fixOutcome, preSHA, dispute string) (bool, error) {
	if r.o.DivergenceScan {
		r.traceFix(round, preSHA, step.head, step.first)
	}
	// An offline run may hold unpushed commits from earlier rounds. Its local
	// gate must pass before those commits or a branch-only disputed head is
	// accepted without its configured test command.
	if r.o.Offline {
		if err := r.ensureTestsGreen(); err != nil {
			return false, err
		}
		afterTestsSHA, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
		if err != nil {
			return false, err
		}
		if afterTestsSHA != step.head {
			r.reviewNext(round, "local test fixes moved the head past the disputed review; reviewing the repaired head")
			return false, nil
		}
		baseUpdated, err := r.ensureBaseCurrent()
		if err != nil {
			return false, err
		}
		if baseUpdated {
			r.reviewNext(round, "base branch update moved the head past the disputed review; reviewing the updated head")
			return false, nil
		}
		if err := r.pushBranchWithFixes(); err != nil {
			return false, err
		}
		if err := r.flushFixComments(); err != nil {
			return false, err
		}
	}
	if r.o.Offline && !r.target.BranchOnly {
		preCISHA := r.headSHA
		if err := r.ensureCIGreen(); err != nil {
			return false, err
		}
		if r.headSHA != preCISHA {
			r.reviewNext(round, "CI repairs moved the head past the disputed review; reviewing the repaired head")
			return false, nil
		}
	}
	baseUpdated, err := r.ensureBaseCurrent()
	if err != nil {
		return false, err
	}
	if baseUpdated {
		return false, r.prepareReviewAfterBaseUpdate(round, "disputed")
	}
	commentURL, commentPosted, err := r.postDisputeComment(
		round, findingsCommentURL(findings), dispute, findings.HeadSHA)
	if err != nil {
		return false, err
	}
	res.Converged = true
	res.DisputeAccepted = true
	res.DisputeText = dispute
	res.DisputeCommentURL = commentURL
	res.DisputeCommentPosted = commentPosted
	return true, nil
}

// reviewNext starts the review of the next round, if the run has one left.
// why, when not empty, says why the current round's review no longer counts.
func (r *run) reviewNext(round int, why string) {
	if why != "" {
		r.rep.Info(why)
	}
	if round < r.o.MaxIter {
		r.startReviewWith(round+1, "")
	}
}

func (r *run) prepareReviewAfterBaseUpdate(round int, reviewed string) error {
	r.reviewNext(round, "base branch update moved the head past the "+reviewed+" review; reviewing the updated head")
	if r.target.BranchOnly || r.o.Offline {
		return nil
	}
	if err := r.ensureCIGreen(); err != nil {
		r.killReview()
		return err
	}
	return nil
}

// suggestionRoundDue keeps Suggestions strictly terminal: the round runs at
// most once, only after a review with zero Blockers and Critical findings, and
// never re-enters the review loop.
func suggestionRoundDue(o Options, f review.Findings) bool {
	return o.FixSuggestions && f.Blocking() == 0 && f.Suggestions > 0
}

// suggestionRound is the one fix step allowed to run after convergence, when
// the clean final review still left Suggestions on the table. It triages
// rather than obeys: leftover Suggestions are often artifacts of the
// reviewers' isolated worktree, and no review follows to catch an overzealous
// change. A round that changes nothing is a legitimate outcome here, not
// ErrNoProgress: no Blockers remain that would demand progress.
func (r *run) suggestionRound(round int, findings review.Findings, comment, preSHA string) (bool, error) {
	r.rep.Step("Suggestion round")
	r.enter(PhaseFix)
	step, err := r.fixStep("suggestion-round", suggestionRoundPrompt(r.prompt, comment))
	if err != nil {
		return false, err
	}
	if step.head == preSHA {
		r.rep.Info("no suggestion was worth a change; the reviewed head stands")
		return false, nil
	}
	r.recordRound("Suggestion round", preSHA)
	if r.o.Offline {
		// The push and the single CI wait follow in finalizeOffline; the test
		// gate is all that checks these commits, because no review does.
		r.queueFixComment(step, "Suggestion round", preSHA)
		if err := r.ensureTestsGreen(); err != nil {
			return true, err
		}
		return true, nil
	}
	if err := r.pushBranchWithFixes(); err != nil {
		return true, err
	}
	if err := r.postFixComment(step, "Suggestion round",
		fmt.Sprintf("Review round %d", round), findingsCommentURL(findings), preSHA); err != nil {
		return true, err
	}
	// The run still ends on this push, so the same barrier as any other round
	// applies: babysit must not report success on a head whose checks are red.
	if !r.target.BranchOnly {
		if err := r.ensureCIGreen(); err != nil {
			return true, err
		}
	}
	return true, nil
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

	r.rep.StepStart(stepLabel(tag), r.fixModel)
	started := time.Now()
	var id string
	err = r.awaitStep(stepLabel(tag), r.fixModel, started, func() error {
		if r.sessionID == "" {
			var callErr error
			id, callErr = r.fixer.Exec(r.ctx, r.env, r.o.FixTimeout,
				firstPrompt(r.rules, r.prCtx, prompt), msgPath, out)
			return callErr
		}
		return r.fixer.Resume(r.ctx, r.env, r.o.FixTimeout, r.sessionID, prompt, msgPath, out)
	})
	if err == nil && id != "" {
		r.sessionID = id
	}
	r.rep.StepEnd(stepLabel(tag), r.fixModel, time.Since(started), err == nil)

	if err != nil {
		// The agent and the CLI both branch on this; wrapping it into the
		// generic exit message would erase the reset time.
		if errors.Is(err, usagelimit.Err) {
			return err
		}
		if errors.Is(err, proc.ErrTimeout) {
			return fmt.Errorf("the fix step timed out after %s (--fix-timeout; 0 disables), see %s",
				r.o.FixTimeout, logPath)
		}
		return fmt.Errorf("the fix session failed with exit %d, see %s", proc.ExitCode(err), logPath)
	}

	data, err := os.ReadFile(msgPath)
	if err != nil || len(strings.TrimSpace(string(data))) == 0 {
		return fmt.Errorf("the fix session produced no final message, see %s", logPath)
	}
	r.lastMsg = string(data)
	return nil
}

// resume is codexCall for steps that must not start a new session.
func (r *run) resume(tag, prompt string) error {
	if r.sessionID == "" {
		return fmt.Errorf("no fix session to resume")
	}
	return r.codexCall(tag, prompt)
}

// readOnlyReviewer runs the read-only passes outside the review rounds, the
// divergence analysis and the final PR description, on the review model.
func (r *run) readOnlyReviewer() (engine.Reviewer, error) {
	return engine.NewReviewer(r.reviewModel, r.o.Bins)
}

// fixOutcome is what one fix step left behind.
type fixOutcome struct {
	tag string
	// head is HEAD once the step and its gates are done.
	head string
	// first is the step's own final message, which carries its PR COMMENT
	// block; last is the newest message, which a gate step may have written.
	first, last string
}

// fixStep runs one step of the shared fix session and its gates: open
// questions are answered, leftover changes committed, and the head read.
func (r *run) fixStep(tag, prompt string) (fixOutcome, error) {
	if err := r.codexCall(tag, prompt); err != nil {
		return fixOutcome{}, err
	}
	return r.finishFixStep(tag)
}

// resumeStep is fixStep for steps that must not start a new session.
func (r *run) resumeStep(tag, prompt string) (fixOutcome, error) {
	if err := r.resume(tag, prompt); err != nil {
		return fixOutcome{}, err
	}
	return r.finishFixStep(tag)
}

func (r *run) finishFixStep(tag string) (fixOutcome, error) {
	step := fixOutcome{tag: tag, first: r.lastMsg}
	if err := r.questionGate(tag); err != nil {
		return step, err
	}
	if err := r.ensureCommitted(tag); err != nil {
		return step, err
	}
	head, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
	if err != nil {
		return step, err
	}
	step.head, step.last = head, r.lastMsg
	return step, nil
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
	return r.requireCleanWorktree(tag)
}

const dirtyStatusPreviewLimit = 10

// requireCleanWorktree records enough evidence to diagnose files a fix session
// left behind. The full status belongs in the run log; the error stays bounded
// because history and the terminal both render it.
func (r *run) requireCleanWorktree(tag string) error {
	status, err := r.p.Git.StatusPorcelain(r.ctx, r.worktree)
	if err != nil {
		return err
	}
	if status == "" {
		return nil
	}

	logPath := filepath.Join(r.logDir, tag+"-dirty.log")
	if err := os.WriteFile(logPath, []byte(status+"\n"), 0o644); err != nil {
		return fmt.Errorf("worktree still has uncommitted changes (%s); inspect %s; could not write full status to %s: %w",
			dirtyStatusPreview(status), r.worktree, logPath, err)
	}
	return fmt.Errorf("worktree still has uncommitted changes (%s); inspect %s; full status: %s",
		dirtyStatusPreview(status), r.worktree, logPath)
}

func dirtyStatusPreview(status string) string {
	lines := strings.Split(status, "\n")
	if len(lines) <= dirtyStatusPreviewLimit {
		return strings.Join(lines, ", ")
	}
	remaining := len(lines) - dirtyStatusPreviewLimit
	return strings.Join(lines[:dirtyStatusPreviewLimit], ", ") +
		fmt.Sprintf(", ... and %d more path(s)", remaining)
}

// pushBranch is the safety net for a push the session forgot, and the barrier
// that keeps a stale check result from passing as green.
//
// After a push, `gh pr checks` can briefly still report the previous head's
// completed checks. A red new commit would then read as green, so nothing is
// trusted until GitHub reports the pushed sha as the PR head.
func (r *run) pushBranch() error {
	g := r.p.Git.WithHooksPath(r.hooksPath)
	var pushedSHA, remoteBefore string
	if err := r.waitActivity("checking the remote head before push", func() error {
		var err error
		pushedSHA, err = r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
		if err != nil {
			return err
		}
		remoteBefore, err = r.remoteHead()
		return err
	}); err != nil {
		return err
	}
	prePushOut, prePushErr := r.prePushWithRetry(g, pushedSHA, remoteBefore)
	if prePushErr != nil {
		logPath := filepath.Join(r.logDir, pushLogFile)
		os.WriteFile(logPath, []byte(prePushOut), 0o644)
		var contention *prePushContention
		if errors.As(prePushErr, &contention) {
			return prePushErr
		}
		return &pushRejection{branch: r.branch, sha: pushedSHA, out: prePushOut, local: true}
	}
	// PrePush verified this exact SHA. Do not run the same hook again during the
	// transport: concurrent linters can make an already-passing check fail on
	// the second run.
	var out string
	pushErr := r.waitActivity("pushing "+r.branch, func() error {
		var err error
		out, err = g.Push(r.ctx, r.worktree, "origin", r.branch, pushedSHA)
		return err
	})
	logPath := filepath.Join(r.logDir, pushLogFile)
	os.WriteFile(logPath, []byte(out), 0o644)

	deadline := time.Now().Add(headSettleTimeout)
	remote, _ := r.remoteHead()
	if pushErr != nil {
		return &pushRejection{branch: r.branch, sha: pushedSHA, out: out}
	}
	if remote == pushedSHA {
		r.headSHA = pushedSHA
		return nil
	}
	r.rep.Info("waiting for GitHub to register the pushed head")
	return r.waitForPushedHead(pushedSHA, deadline)
}

func (r *run) waitForPushedHead(pushedSHA string, deadline time.Time) error {
	return r.waitActivity("waiting for GitHub to register the pushed head", func() error {
		for {
			if err := r.sleep(5 * time.Second); err != nil {
				return err
			}
			remote, _ := r.remoteHead()
			if remote == pushedSHA {
				r.headSHA = pushedSHA
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("origin/%s still reports %s instead of the pushed %s after %s; is someone else pushing to it?",
					r.branch, remote, pushedSHA, headSettleTimeout)
			}
		}
	})
}

type prePushContention struct {
	branch   string
	attempts int
}

func (e *prePushContention) Error() string {
	return fmt.Sprintf("pre-push verification for %s could not start after %d attempts because another golangci-lint process is running",
		e.branch, e.attempts)
}

func (r *run) prePushWithRetry(g git.G, sha, remoteSHA string) (string, error) {
	delay := r.timing.prePushRetryDelay()
	for attempt := 1; ; attempt++ {
		var out string
		err := r.waitActivity("running pre-push verification", func() error {
			var hookErr error
			out, hookErr = g.PrePush(r.ctx, r.worktree, "origin", r.branch, sha, remoteSHA)
			return hookErr
		})
		if err == nil || !transientPrePushFailure(out) {
			return out, err
		}
		if attempt == prePushLockAttempts {
			return out, &prePushContention{branch: r.branch, attempts: prePushLockAttempts}
		}
		r.rep.Info(fmt.Sprintf("pre-push verification is waiting for another golangci-lint process; retrying %d/%d in %s...",
			attempt+1, prePushLockAttempts, delay))
		if err := r.waitActivity("waiting to retry pre-push verification", func() error {
			return r.sleep(delay)
		}); err != nil {
			return out, err
		}
	}
}

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

// postFixComment posts one pushed fix step's log to the PR.
//
// The text comes from the step's PR COMMENT block. One of the step's gates may
// have produced its newest message, so the step's first message is checked as
// well; the commit list is the fallback. Neither is the session's latest
// message: a push fix may have run since, and its block describes the push
// fix, not this step. The pipeline posts it rather than the
// session, which is what keeps it a normal comment from the user.
func (r *run) postFixComment(step fixOutcome, label, reviewLabel, reviewURL, preSHA string) error {
	if r.target.BranchOnly || !r.o.Post {
		return nil
	}
	commits := r.p.Git.LogOneline(r.ctx, r.worktree, preSHA+"..HEAD")
	if commits == "" {
		return nil
	}
	currentSHA, err := r.p.GH.HeadSHA(r.ctx, r.o.RepoRoot, r.pr.Number)
	if err != nil {
		return fmt.Errorf("checking the pull request head before posting the fix log: %w", err)
	}
	if currentSHA != r.headSHA {
		return fmt.Errorf("the pushed fix is for %s but GitHub reports %s; refusing to post the fix log",
			r.headSHA, currentSHA)
	}
	body := fixCommentBody(label, reviewLabel, reviewURL, step.last, step.first, commits)
	r.postPRComment("fix-log comment", body, reviewURL)
	return nil
}

// postDisputeComment posts only the rebuttal that survived the dispute gate.
// Earlier claims are deliberately kept off the PR because the adversarial
// re-check may still prove them wrong.
func (r *run) postDisputeComment(round int, reviewURL, dispute, reviewedSHA string) (string, bool, error) {
	if r.target.BranchOnly || !r.o.Post {
		return "", false, nil
	}
	body := disputeCommentBody(round, reviewURL, dispute)
	if body == "" {
		url, posted := r.postPRComment("rebuttal", body, "")
		return url, posted, nil
	}
	currentSHA, err := r.p.GH.HeadSHA(r.ctx, r.o.RepoRoot, r.pr.Number)
	if err != nil {
		return "", false, fmt.Errorf("checking the pull request head before posting the rebuttal: %w", err)
	}
	if currentSHA != reviewedSHA {
		return "", false, fmt.Errorf("the review is for %s but GitHub reports %s; refusing to post the rebuttal",
			reviewedSHA, currentSHA)
	}
	if reviewURL == "" {
		r.rep.Warn(fmt.Sprintf("review round %d has no comment URL; posting the rebuttal without a backlink", round))
	}
	url, posted := r.postPRComment("rebuttal", body, reviewURL)
	return url, posted, nil
}

func (r *run) postPRComment(kind, body, generatedURL string) (string, bool) {
	if !r.o.Post {
		return "", false
	}
	if strings.TrimSpace(body) == "" {
		r.rep.Warn(fmt.Sprintf("could not post the %s to PR #%d: the comment body is empty", kind, r.pr.Number))
		return "", false
	}
	if term := prohibitedPRCommentTermExcept(body, generatedURL); term != "" {
		r.rep.Warn(fmt.Sprintf("could not post the %s to PR #%d: the comment contains prohibited term %q", kind, r.pr.Number, term))
		return "", false
	}
	url, err := r.p.GH.Comment(r.ctx, r.o.RepoRoot, r.pr.Number, body)
	if err != nil {
		r.rep.Warn(fmt.Sprintf("could not post the %s to PR #%d: %v", kind, r.pr.Number, err))
		return "", false
	}
	return url, true
}

func prohibitedPRCommentTermExcept(text, generatedURL string) string {
	if generatedURL != "" {
		text = strings.ReplaceAll(text, generatedURL, " ")
	}
	words := strings.FieldsFunc(text, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	for i, word := range words {
		switch strings.ToLower(word) {
		case "ai", "openai", "agent", "agents", "codex", "automation":
			return word
		}
		if i+1 < len(words) && strings.EqualFold(word, "artificial") &&
			strings.EqualFold(words[i+1], "intelligence") {
			return word + " " + words[i+1]
		}
	}
	return ""
}

func fixCommentBody(label, reviewLabel, reviewURL, current, original, commits string) string {
	body := markerContent(current, markerComment)
	if body == "" {
		body = markerContent(original, markerComment)
	}
	if body == "" {
		body = fmt.Sprintf("Commits:\n\n```text\n%s\n```", strings.TrimSpace(commits))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "### %s", label)
	if reviewURL != "" && reviewLabel != "" {
		fmt.Fprintf(&b, "\n\n[%s](%s)", reviewLabel, reviewURL)
	}
	fmt.Fprintf(&b, "\n\n%s", body)
	return b.String()
}

func disputeCommentBody(round int, reviewURL, dispute string) string {
	body := markerContent(dispute, markerDisputed)
	if body == "" {
		return ""
	}
	title := fmt.Sprintf("review round %d", round)
	if reviewURL != "" {
		title = fmt.Sprintf("[%s](%s)", title, reviewURL)
	}
	return fmt.Sprintf("### Rebuttal to %s\n\n%s", title, body)
}

func markerContent(text, marker string) string {
	block := section(text, marker)
	if block == "" {
		return ""
	}
	_, body, ok := strings.Cut(block, "\n")
	if !ok {
		return ""
	}
	return strings.TrimSpace(body)
}

func findingsCommentURL(findings review.Findings) string {
	if findings.CommentURL == nil {
		return ""
	}
	return *findings.CommentURL
}

// cleanup stops a background review and removes a successful run's worktree.
// Failed worktrees stay available for diagnosis until normal cache collection.
func (r *run) cleanup(succeeded bool) {
	r.killReview()
	if succeeded && !r.o.KeepWorktree {
		r.p.Git.WorktreeRemove(r.ctx, r.o.RepoRoot, r.worktree)
	}
	r.releaseRunClaim()
}

// releaseRunClaim ends the run's ownership. The private TMPDIR goes first and
// on every outcome: unlike a failed worktree, nothing in it is kept for later.
func (r *run) releaseRunClaim() {
	if r.removeTmp != nil {
		r.removeTmp()
		r.removeTmp = nil
	}
	if r.releaseClaim != nil {
		r.releaseClaim()
		r.releaseClaim = nil
	}
}

func (o Options) withDefaults() Options {
	if o.MaxIter == 0 {
		o.MaxIter = defaultMaxIter
	}
	if o.MaxCIFixes == 0 {
		o.MaxCIFixes = defaultMaxCIFixes
	}
	if o.FixTimeout == 0 {
		o.FixTimeout = defaultFixTimeout
	}
	return o
}

func (o Options) validate() error {
	if o.MaxIter < 1 {
		return fmt.Errorf("max-iter must be >= 1")
	}
	if o.Local && o.Number > 0 {
		return fmt.Errorf("a local run works on the current branch; it cannot take a pull request")
	}
	if o.DivergenceScan {
		for _, target := range o.DivergenceEscalateTo {
			if !validDivergenceTarget(target) {
				return fmt.Errorf("invalid divergence escalation target %q; expected user or org/team without @", target)
			}
		}
	}
	if !spec.Valid(o.Engine) {
		return fmt.Errorf("engine must be %s", spec.Choices())
	}
	if !spec.Valid(o.Review.Engine) {
		return fmt.Errorf("review-engine must be %s", spec.Choices())
	}
	if !spec.ValidEffort(o.Engine, o.Effort) {
		return fmt.Errorf("effort must be one of: %s", strings.Join(spec.Efforts(o.Engine), ", "))
	}
	if !spec.ValidEffort(o.Review.Engine, o.Review.Effort) {
		return fmt.Errorf("review-effort must be one of: %s", strings.Join(spec.Efforts(o.Review.Engine), ", "))
	}
	if o.DivergenceTimeout < 0 {
		return fmt.Errorf("divergence timeout must not be negative")
	}
	if o.Repo == "" || o.RepoRoot == "" {
		return fmt.Errorf("repo and repo root are required")
	}
	return nil
}

func (p *Pipeline) reporter() Reporter {
	if p.Rep == nil {
		return nopReporter{}
	}
	return p.Rep
}

func stepLabel(tag string) string {
	// Gate follow-ups hang off their round's tag: fix-round-2-dispute-1 is
	// the first forced re-check of round 2, fix-round-2-answers-1 its first
	// answer to open questions. The round number is already on the screen
	// above them, so the label names only the follow-up.
	if rest, ok := strings.CutPrefix(tag, "fix-round-"); ok {
		if _, n, ok := strings.Cut(rest, "-dispute-"); ok {
			return "Dispute re-check " + n
		}
		if _, n, ok := strings.Cut(rest, "-answers-"); ok {
			return "Answers " + n
		}
	}
	switch {
	case strings.HasPrefix(tag, "fix-round-"):
		return "Fix round " + strings.TrimPrefix(tag, "fix-round-")
	case strings.HasPrefix(tag, "push-fix-"):
		return "Push fix " + strings.TrimPrefix(tag, "push-fix-")
	case strings.HasPrefix(tag, "ci-fix-"):
		return "CI fix " + strings.TrimPrefix(tag, "ci-fix-")
	case strings.HasPrefix(tag, "test-fix-"):
		return "Test fix " + strings.TrimPrefix(tag, "test-fix-")
	case strings.HasPrefix(tag, "suggestion-round"):
		return "Suggestion round" + strings.TrimPrefix(tag, "suggestion-round")
	default:
		return "Codex " + tag
	}
}

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
	"unicode"

	"github.com/yungweng/quorum/internal/cachefs"
	"github.com/yungweng/quorum/internal/engine"
	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/proc"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/runname"
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
	// ErrNoProgress is exit 5: a fix round changed nothing while findings
	// remain, and did not dispute them either.
	ErrNoProgress = errors.New("fix round produced no changes although findings remain")
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
	// Rules is the repository's user-local review rules file, verbatim. Review
	// rounds hand it to every reviewer pass, the aggregator and the verifier;
	// fix sessions get it appended to their standing rules so a fix cannot
	// resolve a finding by violating the rule that produced it.
	Rules string

	Engine string // fix-session engine: codex, claude or grok; empty means codex
	Model  string
	Effort string

	Reviewers    int
	ReviewEngine string // review-round engine: codex, claude or grok; empty means codex
	ReviewModel  string
	ReviewEffort string

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
	// ResolveConflicts merges origin/<base> through the fix session whenever
	// the branch conflicts with its base, before any review round.
	ResolveConflicts bool
	// FixSuggestions runs one terminal fix round when the final review is
	// clean but still lists Suggestions: triage each one, implement what is
	// worth it, and end the run without another review.
	FixSuggestions bool
	// ResumeRun reuses a prior review run's completed reviewer output for the
	// first review round, the way a failed round's in-run retry does. Later
	// rounds always fan out fresh: they review a head the saved output has
	// never seen.
	ResumeRun string

	// Bypass runs the fix sessions with --dangerously-bypass-approvals-and-
	// sandbox. They must run tests, use gh and push, unattended, and a
	// sandboxed exec would silently skip exactly those commands.
	Bypass      bool
	Interactive bool
	UseDirenv   bool
	// AllowEnvrcChange permits direnv to load an .envrc changed by the target.
	AllowEnvrcChange bool
	KeepWorktree     bool

	// Verbose streams the full Codex output instead of only the status line.
	// The logs are written either way; this decides what reaches the terminal.
	Verbose bool
	// Out is where verbose output goes. Nil discards it.
	Out io.Writer

	RunsDir       string
	ReviewRunsDir string
	DepsDir       string
	CodexBin      string
	ClaudeBin     string
	GrokBin       string
	DirenvBin     string
}

// engineBin picks the resolved binary for name.
func (o Options) engineBin(name string) string {
	switch name {
	case engine.Claude:
		return o.ClaudeBin
	case engine.Grok:
		return o.GrokBin
	default:
		return o.CodexBin
	}
}

// RoundEntry is one round's commit list, for the closing summary.
type RoundEntry struct {
	Label   string
	Commits string
}

// Result is what a finished pipeline run produced.
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

// Pipeline runs the review-fix cycle.
type Pipeline struct {
	GH     *gh.Client
	Git    git.G
	Review Reviewer
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
	succeeded := false
	defer func() { r.cleanup(succeeded) }()

	res, err := r.execute()
	succeeded = err == nil
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
	headSHA      string
	root         string
	worktree     string
	logDir       string
	msgDir       string
	releaseClaim func()

	env   envexec.Env
	fixer engine.Fixer
	rules string
	prCtx string

	// The two models this run works with, resolved once in prepare. Every step
	// reports one of them, and the review side is also what the divergence scan
	// and the final PR description run on.
	reviewModel engine.Model
	fixModel    engine.Model

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
	testFixTotal    int
	pushFixTotal    int
	testRuns        int
	conflictFixes   int
	disputeAccepted bool
	disputeText     string
	divergenceTrace DivergenceTrace

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

// prepare resolves the PR or current pushed branch, checks the preconditions
// and sets up the worktree.
func (r *run) prepare() error {
	var tgt target.Target
	var err error
	if r.o.Local {
		tgt, err = target.ResolveLocal(r.ctx, r.p.GH, r.p.Git, r.o.RepoRoot, "")
	} else {
		tgt, err = target.Resolve(r.ctx, r.p.GH, r.p.Git, r.o.RepoRoot, r.o.Number, "", "")
	}
	if err != nil {
		return err
	}
	pr := tgt.PR
	if !tgt.BranchOnly && pr.State != "OPEN" {
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

	stamp := time.Now().Format("20060102-150405")
	targetName := fmt.Sprintf("pr-%d", pr.Number)
	if tgt.BranchOnly {
		targetName = "branch-" + runname.BranchPart(r.branch)
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

	// The review side is reported resolved, not as configured: an empty review
	// model becomes the engine's default here in quorum, so passing the raw
	// value on would announce "engine default" for a run that is about to use a
	// model this code already picked. The fix side stays as configured, because
	// an empty one really is the CLI's own choice and quorum never learns it.
	reviewEngine, reviewModel, reviewEffort := review.ReviewEngineDefaults(
		r.o.ReviewEngine, r.o.ReviewModel, r.o.ReviewEffort)
	r.reviewModel = engine.Model{Engine: reviewEngine, Name: reviewModel, Effort: reviewEffort}
	r.fixModel = engine.Model{Engine: r.o.Engine, Name: r.o.Model, Effort: r.o.Effort}

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

	r.env = envexec.Env{Worktree: r.worktree, Direnv: r.o.UseDirenv, DirenvBin: r.o.DirenvBin}
	fixer, err := engine.NewFixer(r.o.Engine, engine.FixerOptions{
		Bin: r.o.engineBin(r.o.Engine), Model: r.o.Model, Effort: r.o.Effort, Bypass: r.o.Bypass,
	})
	if err != nil {
		return err
	}
	r.fixer = fixer
	r.rules = standingRules(r.branch, !r.o.Interactive, tgt.BranchOnly, r.o.Offline, r.o.Rules)
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
		Engine: r.o.Engine, Model: r.o.Model, Effort: r.o.Effort, Bypass: r.o.Bypass,
		ReviewEngine: r.o.ReviewEngine, ReviewModel: reviewModel, ReviewEffort: reviewEffort,
		Interactive: r.o.Interactive, Offline: r.o.Offline, TestCmd: r.o.TestCmd, TestCmdNote: testCmdNote,
		MaxIter: r.o.MaxIter, MaxCIFixes: r.o.MaxCIFixes,
		DivergenceScan: r.o.DivergenceScan,
		FixTimeout:     r.o.FixTimeout, RunDir: r.root, Worktree: r.worktree, HeadSHA: headSHA,
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

// execute is the main loop.
func (r *run) execute() (*Result, error) {
	res := &Result{PR: r.pr, BranchOnly: r.target.BranchOnly, Local: r.o.Local, RunDir: r.root}
	if r.o.DivergenceScan {
		r.startDivergenceTrace()
	}

	// Conflicts are handled before anything else: a conflicted PR cannot merge,
	// GitHub never even starts its pull_request checks, and a review of the
	// unmerged head would polish code that cannot land as it is.
	if err := r.ensureMergeable(); err != nil {
		return res, err
	}

	// A conflict fix above moved the head, so a handed-in resume would replay
	// reviewer output for a head that no longer exists; only an untouched head
	// may reuse it.
	resume := r.o.ResumeRun
	if r.conflictFixes > 0 {
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
				if iteration < r.o.MaxIter {
					r.startReview(iteration + 1)
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

		r.rep.Step(fmt.Sprintf("Fix round %d/%d", iteration, r.o.MaxIter))
		r.enter(PhaseFix)
		preFixSHA := currentSHA
		tag := fmt.Sprintf("fix-round-%d", iteration)

		if err := r.codexCall(tag, fixRoundPrompt(r.pr.Number, r.branch, r.target.BranchOnly, r.o.Offline, comment)); err != nil {
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
				if r.o.DivergenceScan {
					r.traceFix(iteration, preFixSHA, afterSHA, tag)
				}
				// An offline run may hold unpushed commits from earlier rounds.
				// Its local gate must pass before those commits or a branch-only
				// disputed head is accepted without its configured test command.
				if r.o.Offline {
					if err := r.ensureTestsGreen(); err != nil {
						return res, err
					}
					afterTestsSHA, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
					if err != nil {
						return res, err
					}
					if afterTestsSHA != afterSHA {
						r.rep.Info("local test fixes moved the head past the disputed review; reviewing the repaired head")
						if iteration < r.o.MaxIter {
							r.startReview(iteration + 1)
						}
						continue
					}
					conflictFixes := r.conflictFixes
					if err := r.ensureMergeable(); err != nil {
						return res, err
					}
					if r.conflictFixes != conflictFixes {
						r.rep.Info("merge conflict resolution moved the head past the disputed review; reviewing the resolved head")
						if iteration < r.o.MaxIter {
							r.startReview(iteration + 1)
						}
						continue
					}
					if err := r.pushBranchWithFixes(); err != nil {
						return res, err
					}
					if err := r.flushFixComments(); err != nil {
						return res, err
					}
				}
				if r.o.Offline && !r.target.BranchOnly {
					preCISHA := r.headSHA
					if err := r.ensureCIGreen(); err != nil {
						return res, err
					}
					if r.headSHA != preCISHA {
						r.rep.Info("CI repairs moved the head past the disputed review; reviewing the repaired head")
						if iteration < r.o.MaxIter {
							r.startReview(iteration + 1)
						}
						continue
					}
				}
				commentURL, commentPosted, err := r.postDisputeComment(
					iteration, findingsCommentURL(findings), r.disputeText, findings.HeadSHA)
				if err != nil {
					return res, err
				}
				res.Converged = true
				res.DisputeAccepted = true
				res.DisputeText = r.disputeText
				res.DisputeCommentURL = commentURL
				res.DisputeCommentPosted = commentPosted
				break
			}
			// The dispute gate only returns unaccepted once new commits exist.
			afterSHA, err = r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
			if err != nil {
				return res, err
			}
		}
		if r.o.DivergenceScan {
			r.traceFix(iteration, preFixSHA, afterSHA, tag)
		}

		r.recordRound(fmt.Sprintf("Review fix round %d", iteration), preFixSHA)
		if r.o.Offline {
			// The commits stay local: the comment is queued for the final
			// push, and the test gate replaces this round's CI wait.
			r.queueFixComment(tag, fmt.Sprintf("Review fix round %d", iteration), preFixSHA)
		} else {
			if err := r.pushBranchWithFixes(); err != nil {
				return res, err
			}
			if err := r.postFixComment(tag, fmt.Sprintf("Review fix round %d", iteration),
				fmt.Sprintf("Review round %d", iteration), findingsCommentURL(findings), preFixSHA); err != nil {
				return res, err
			}
		}

		// The base may have moved during the round. Re-checking here, while no
		// review is running, keeps a late conflict from surviving to the merge.
		if err := r.ensureMergeable(); err != nil {
			return res, err
		}
		if r.o.Offline {
			if err := r.ensureTestsGreen(); err != nil {
				return res, err
			}
			// Test repairs can move the local head after the first check above.
			// Refresh the base before starting a review of those new commits.
			if err := r.ensureMergeable(); err != nil {
				return res, err
			}
		}

		// Overlap the next review with this round's CI wait.
		if iteration < r.o.MaxIter {
			r.startReview(iteration + 1)
		}
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
	tag := "suggestion-round"
	if err := r.codexCall(tag, suggestionRoundPrompt(r.pr.Number, r.branch, r.target.BranchOnly, r.o.Offline, comment)); err != nil {
		return false, err
	}
	if err := r.questionGate(tag); err != nil {
		return false, err
	}
	if err := r.ensureCommitted(tag); err != nil {
		return false, err
	}
	afterSHA, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
	if err != nil {
		return false, err
	}
	if afterSHA == preSHA {
		r.rep.Info("no suggestion was worth a change; the reviewed head stands")
		return false, nil
	}
	r.recordRound("Suggestion round", preSHA)
	if r.o.Offline {
		// The push and the single CI wait follow in finalizeOffline; the test
		// gate is all that checks these commits, because no review does.
		r.queueFixComment(tag, "Suggestion round", preSHA)
		if err := r.ensureTestsGreen(); err != nil {
			return true, err
		}
		return true, nil
	}
	if err := r.pushBranchWithFixes(); err != nil {
		return true, err
	}
	if err := r.postFixComment(tag, "Suggestion round",
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

	if r.sessionID == "" {
		var id engine.SessionRef
		id, err = r.fixer.Exec(r.ctx, r.env, r.o.FixTimeout,
			firstPrompt(r.rules, r.prCtx, prompt), msgPath, out)
		if err == nil {
			r.sessionID = id
		}
	} else {
		err = r.fixer.Resume(r.ctx, r.env, r.o.FixTimeout, r.sessionID, prompt, msgPath, out)
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
			r.headSHA = pushedSHA
			return nil
		}
		if pushErr != nil {
			hookOut, hookErr := r.p.Git.PrePush(r.ctx, r.worktree, "origin", r.branch, pushedSHA)
			return &pushRejection{branch: r.branch, sha: pushedSHA, out: hookOut, local: hookErr != nil}
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

// postFixComment posts one pushed fix step's log to the PR.
//
// The text comes from the session's PR COMMENT block. A later gate step may
// have produced the newest message, so the step's own message is checked as
// well; the commit list is the fallback. The pipeline posts it rather than the
// session, which is what keeps it a normal comment from the user.
func (r *run) postFixComment(tag, label, reviewLabel, reviewURL, preSHA string) error {
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
	var original string
	if data, err := os.ReadFile(filepath.Join(r.msgDir, tag+".md")); err == nil {
		original = string(data)
	}
	body := fixCommentBody(label, reviewLabel, reviewURL, r.lastMsg, original, commits)
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
	url, err := r.p.GH.CommentBody(r.ctx, r.o.RepoRoot, r.pr.Number, body)
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
	body := markerContent(current, MarkerComment)
	if body == "" {
		body = markerContent(original, MarkerComment)
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
	body := markerContent(dispute, MarkerDisputed)
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
		_ = cachefs.RemoveAll(dir)
	}
	r.p.Git.WorktreePrune(r.ctx, r.o.RepoRoot)
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
	if !engine.Valid(o.Engine) {
		return fmt.Errorf("engine must be %s, %s or %s", engine.Codex, engine.Claude, engine.Grok)
	}
	if !engine.Valid(o.ReviewEngine) {
		return fmt.Errorf("review-engine must be %s, %s or %s", engine.Codex, engine.Claude, engine.Grok)
	}
	if !engine.ValidEffort(o.Engine, o.Effort) {
		return fmt.Errorf("effort must be one of: %s", strings.Join(engine.Efforts(o.Engine), ", "))
	}
	if !engine.ValidEffort(o.ReviewEngine, o.ReviewEffort) {
		return fmt.Errorf("review-effort must be one of: %s", strings.Join(engine.Efforts(o.ReviewEngine), ", "))
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
	case strings.HasPrefix(tag, "test-fix-"):
		return "Test fix " + strings.TrimPrefix(tag, "test-fix-")
	case strings.HasPrefix(tag, "suggestion-round"):
		return "Suggestion round" + strings.TrimPrefix(tag, "suggestion-round")
	default:
		return "Codex " + tag
	}
}

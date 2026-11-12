package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/usagelimit"
)

// Reviewer is the subset of review.Runner the pipeline depends on.
// *review.Runner satisfies it as-is; the seam exists so tests can control one
// round's outcome without spinning up real git, gh and an engine.
type Reviewer interface {
	Run(ctx context.Context, o review.Options) (*review.Result, error)
}

// bgReview is a review running while the pipeline waits for CI.
type bgReview struct {
	round   int
	resume  string // run directory this round reuses; empty for a fresh fan-out
	started time.Time
	cancel  context.CancelFunc
	done    chan bgResult
	// outcome is how the review ended, one of the SideReview values; unset
	// while it runs. The CI wait reads it without taking the result off done.
	outcome atomic.Value
}

type bgResult struct {
	res *review.Result
	err error
}

// startReviewWith kicks off a review in the background, reusing resumeDir's
// completed reviewer output when it is non-empty.
//
// A review only needs the pushed PR head, not green checks, so it overlaps the
// CI wait instead of queueing behind it. It must never overlap a fix
// session: a fix moves the head sha out from under the findings, which is why
// ensureCIGreen cancels a running review before it starts one.
func (r *run) startReviewWith(round int, resumeDir string) {
	if r.review != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.ctx)
	br := &bgReview{
		round:   round,
		resume:  resumeDir,
		started: time.Now(),
		cancel:  cancel,
		done:    make(chan bgResult, 1),
	}
	r.review = br

	opts := r.reviewOptions()
	opts.ResumeRun = resumeDir
	go func() {
		res, err := r.p.Review.Run(ctx, opts)
		switch {
		case err != nil:
			br.outcome.Store(sideReviewFailed)
		case res != nil && res.Posted:
			br.outcome.Store(SideReviewPosted)
		default:
			br.outcome.Store(sideReviewFinished)
		}
		br.done <- bgResult{res, err}
	}()
}

func (r *run) reviewOptions() review.Options {
	o := r.o.Review
	o.Repo = r.o.Repo
	o.Number = r.pr.Number
	o.Branch = branchOnlyValue(r.target.BranchOnly, r.branch)
	o.HeadSHA = branchOnlyValue(r.target.BranchOnly, r.headSHA)
	o.LocalHead = false
	o.LocalPR = nil
	o.RepoRoot = r.o.RepoRoot
	o.Rules = r.o.Rules
	o.BaseBranch = r.pr.BaseRefName
	o.Post = r.o.Post && !r.target.BranchOnly
	o.KeepWorktree = false
	o.UseDirenv = r.o.UseDirenv
	o.AllowEnvrcChange = r.o.AllowEnvrcChange
	o.ResumeRun = ""
	o.DepsDir = r.o.DepsDir
	o.Bins = r.o.Bins
	o.DirenvBin = r.o.DirenvBin
	if r.o.Offline {
		// Offline rounds review the worktree's unpushed head. An unresolvable
		// HEAD leaves HeadSHA empty, which the review's own validation refuses
		// loudly instead of silently reviewing origin's older head.
		head, _ := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
		o.LocalHead = true
		if !r.target.BranchOnly {
			o.LocalPR = &r.pr
		}
		o.Branch = r.branch
		o.HeadSHA = head
		o.BaseBranch = r.pr.BaseRefName
		o.Post = false
	}
	return o
}

func branchOnlyValue(branchOnly bool, branch string) string {
	if branchOnly {
		return branch
	}
	return ""
}

// finishReview joins the running review and returns its findings plus the
// comment text that goes into the next fix round. When a round fails after
// its reviewers already produced output - an aggregator or verifier failure,
// or a usage limit - it is retried exactly once, resuming that output instead
// of paying for a fresh fan-out. A round that was handed a resume directory
// and failed because that directory was unusable is retried once with a fresh
// fan-out instead. Reviewer-phase failures and a drifted head are not
// retried: a resume fixes neither.
func (r *run) finishReview(round int) (review.Findings, string, error) {
	if r.review == nil {
		r.startReviewWith(round, "")
	}
	br := r.review
	label := fmt.Sprintf("Review round %d", round)
	r.rep.StepStart(label, r.reviewModel)

	out := r.waitReview(br, label)
	r.review = nil
	if out.err != nil {
		if br.resume != "" && review.ResumeFallbackEligible(out.err) {
			// The handed-in directory was unusable, which says nothing about
			// the pull request; one fresh fan-out replaces it, mirroring the
			// agent's fallback for a stale cross-poll resume.
			r.rep.Info(fmt.Sprintf("resume of %s failed (%v); retrying once with a fresh reviewer fan-out", br.resume, out.err))
			r.startReviewWith(round, "")
			out = r.waitReview(r.review, fmt.Sprintf("Review round %d (fresh)", round))
			r.review = nil
		} else if resumeDir, ok := resumableRunDir(out.err); ok {
			r.rep.Info(fmt.Sprintf("review round %d failed (%v); retrying once with its completed reviewer output", round, out.err))
			r.startReviewWith(round, resumeDir)
			out = r.waitReview(r.review, fmt.Sprintf("Review round %d (resumed)", round))
			r.review = nil
		}
		if out.err != nil {
			return review.Findings{}, "", fmt.Errorf("review round %d failed: %w", round, out.err)
		}
	}

	return out.res.Findings, out.res.CommentBody, nil
}

// resumableRunDir reports whether err is a round failure whose completed
// reviewer output is worth resuming, and where it lives. Beyond what review
// counts as resumable, a usage limit is too: it says nothing about the output
// the round completed before it.
func resumableRunDir(err error) (string, bool) {
	if dir, ok := review.ResumableRunDir(err); ok {
		return dir, true
	}
	if !errors.Is(err, usagelimit.Err) {
		return "", false
	}
	dir := review.RunDirOf(err)
	return dir, dir != ""
}

// waitReview blocks on a background review while the status line ticks. The
// review's context derives from the run's, so a cancelled run ends it too.
func (r *run) waitReview(br *bgReview, label string) bgResult {
	var out bgResult
	err := r.awaitStep(label, r.reviewModel, br.started, func() error {
		out = <-br.done
		return out.err
	})
	if r.ctx.Err() != nil {
		br.cancel()
		return out
	}
	r.rep.StepEnd(label, r.reviewModel, time.Since(br.started), err == nil)
	return out
}

// discardReview cancels a review whose findings a CI fix is about to invalidate
// and waits for its processes to stop before the fix session starts.
func (r *run) discardReview() {
	br := r.review
	if br == nil {
		return
	}
	r.review = nil
	r.rep.Info("cancelling the review of the outgoing head")
	br.cancel()
	<-br.done
	r.rep.StepEnd(fmt.Sprintf("Review round %d (cancelled)", br.round), r.reviewModel,
		time.Since(br.started), true)
}

// killReview cancels a running review outright. Used on the paths that end the
// run, where nothing is left to wait for.
func (r *run) killReview() {
	br := r.review
	if br == nil {
		return
	}
	r.review = nil
	br.cancel()
	<-br.done
}

func (r *run) ensureCIGreen() error {
	for attempt := 0; ; {
		r.prog.CI = CIWaiting
		r.enter(PhaseCI)
		waitStarted := time.Now()
		state, err := r.awaitChecks(waitStarted)
		if err != nil {
			return err
		}
		switch state {
		case gh.ChecksPass:
			r.rep.CIGreen(time.Since(waitStarted))
			r.prog.CI = CIGreen
			r.publish()
			return nil
		case gh.ChecksNone:
			r.rep.Warn("no CI checks reported on this PR; continuing")
			r.prog.CI = CINone
			r.publish()
			return nil
		}

		attempt++
		if attempt > r.o.MaxCIFixes {
			r.rep.Notify("CI rot", fmt.Sprintf("PR #%d nach %d Fix-Versuchen weiter rot", r.pr.Number, r.o.MaxCIFixes))
			return fmt.Errorf("%w (%d attempts): %s", ErrCIRed, r.o.MaxCIFixes, r.pr.URL)
		}
		r.rep.CIRed(attempt, r.o.MaxCIFixes)
		r.prog.CI = CIRed
		r.prog.CIFix = attempt
		r.publish()

		// The fix moves the head sha, so any review of the outgoing head is
		// about to describe code that no longer exists.
		r.discardReview()

		fails, err := r.p.GH.FailingChecks(r.ctx, r.o.RepoRoot, r.pr.Number)
		if err != nil {
			return err
		}
		failsJSON, _ := json.MarshalIndent(fails, "", "  ")

		preSHA, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
		if err != nil {
			return err
		}
		fixNumber := r.ciFixTotal + 1
		r.enter(PhaseCIFix)
		step, err := r.fixStep(fmt.Sprintf("ci-fix-%d", fixNumber), ciFixPrompt(r.pr.Number, string(failsJSON)))
		if err != nil {
			return err
		}
		r.ciFixTotal = fixNumber
		label := fmt.Sprintf("CI fix %d", fixNumber)
		r.recordRound(label, preSHA)
		if err := r.pushBranchWithFixes(); err != nil {
			return err
		}
		if r.o.DivergenceScan {
			r.traceCIFix(preSHA, r.headSHA, step.first)
		}
		if err := r.postFixComment(step, label, "", "", preSHA); err != nil {
			return err
		}
	}
}

// awaitChecks runs watchCI in the background so the reporter can tick the CI
// wait the way waitReview ticks a review: `gh pr checks --watch` blocks for as
// long as CI runs, so the elapsed time cannot come from inside the poll loop.
// On cancellation the wait joins watchCI, whose own ctx handling makes it
// return.
func (r *run) awaitChecks(started time.Time) (gh.CheckState, error) {
	defer func() { r.prog.SideReview = "" }()
	var state gh.CheckState
	err := r.await(func(elapsed time.Duration) {
		if elapsed > 0 {
			elapsed = time.Since(started)
		}
		r.rep.CIWait(r.pr.Number, elapsed, r.sideReview())
	}, func() error {
		var err error
		state, err = r.watchCI()
		return err
	})
	return state, err
}

// sideReview describes the review running beside a CI wait, for the wait's
// status line, and publishes its state when that changed. Without it a run
// whose review already posted its comment reads as one that only waits.
func (r *run) sideReview() string {
	br := r.review
	if br == nil {
		return ""
	}
	state, ok := br.outcome.Load().(string)
	if !ok {
		state = sideReviewRunning
	}
	if r.prog.SideReview != state {
		r.prog.SideReview = state
		r.publish()
	}
	return fmt.Sprintf("review round %d %s", br.round, state)
}

// watchCI blocks until the checks settle and GitHub confirms them for the
// exact head.
//
// `gh pr checks` only knows the checks GitHub has registered so far. Right
// after a push that can be a check set made only of external apps, with the
// Actions runs still to come, and `--watch` then reports it green. So a green
// (or empty) answer is only accepted once confirmCI, bound to the head sha,
// agrees: every required check of the base branch reported and passed, every
// Actions run of the head finished successfully, and Actions had its
// registration window if no run of the head exists yet.
func (r *run) watchCI() (gh.CheckState, error) {
	cw := &ciWait{started: time.Now()}
	for {
		state, out, err := r.p.GH.WatchChecks(r.ctx, r.o.RepoRoot, r.pr.Number)
		writeTail(filepath.Join(r.logDir, "ci-last.log"), out, 20)
		if err != nil {
			if errors.Is(err, gh.ErrTransient) {
				if err := r.sleep(time.Second); err != nil {
					return state, err
				}
				continue
			}
			return state, err
		}

		switch state {
		case gh.ChecksPending:
			if err := r.sleep(r.timing.ciPending()); err != nil {
				return state, err
			}
			continue
		case gh.ChecksFail:
			return state, nil
		}

		confirmed, err := r.confirmCI(cw)
		if err != nil {
			if errors.Is(err, gh.ErrTransient) {
				if err := r.sleep(time.Second); err != nil {
					return gh.ChecksPending, err
				}
				continue
			}
			return gh.ChecksPending, err
		}
		switch confirmed {
		case gh.ChecksPending:
			poll := r.timing.ciPending()
			if state == gh.ChecksNone {
				poll = r.timing.ciNoChecks()
			}
			if err := r.sleep(poll); err != nil {
				return confirmed, err
			}
		default:
			return confirmed, nil
		}
	}
}

// ciWait is what one CI wait remembers between confirmations.
type ciWait struct {
	started time.Time
	// required is read once per wait: branch rules do not change while CI runs.
	required    []string
	requiredSet bool
	// hasWorkflows is asked only when the head has no Actions run yet.
	hasWorkflows    bool
	hasWorkflowsSet bool
	announced       bool
}

// confirmCI judges the checks of the exact head sha. It answers ChecksPass,
// ChecksFail (a reported check failed; the CI fix path takes it from there),
// ChecksNone (after the registration window, nothing exists and nothing is
// required), or ChecksPending. A state no fix session can repair - an Actions
// run that failed without reporting any check, or a required check that never
// reported - ends the wait with ErrCIUnconfirmed instead of reading as green.
func (r *run) confirmCI(cw *ciWait) (gh.CheckState, error) {
	sha := r.headSHA
	if sha == "" {
		return gh.ChecksPending, errors.New("confirming CI: the pipeline has no head sha")
	}
	if !cw.requiredSet {
		required, err := r.p.GH.RequiredChecks(r.ctx, r.o.Repo, r.pr.BaseRefName)
		if err != nil {
			return gh.ChecksPending, fmt.Errorf("reading the required checks of %s: %w", r.pr.BaseRefName, err)
		}
		cw.required, cw.requiredSet = required, true
	}
	checks, err := r.p.GH.CommitChecks(r.ctx, r.o.Repo, sha)
	if err != nil {
		return gh.ChecksPending, fmt.Errorf("reading the checks of %s: %w", sha, err)
	}
	runs, err := r.p.GH.WorkflowRuns(r.ctx, r.o.Repo, sha)
	if err != nil {
		return gh.ChecksPending, fmt.Errorf("reading the workflow runs of %s: %w", sha, err)
	}
	inWindow := time.Since(cw.started) < r.timing.noChecksWindow()

	bucket := make(map[string]string, len(checks))
	for _, ch := range checks {
		if ch.Bucket == gh.BucketFail {
			return gh.ChecksFail, nil
		}
		bucket[ch.Name] = ch.Bucket
	}

	var waiting, missing, failedRuns []string
	for _, name := range cw.required {
		switch b, ok := bucket[name]; {
		case !ok:
			missing = append(missing, name)
		case b == gh.BucketPending:
			waiting = append(waiting, name)
		}
	}
	for _, ch := range checks {
		if ch.Bucket == gh.BucketPending && !slices.Contains(waiting, ch.Name) {
			waiting = append(waiting, ch.Name)
		}
	}
	for _, run := range runs {
		switch run.Bucket() {
		case gh.BucketPending:
			waiting = append(waiting, "workflow "+run.Name)
		case gh.BucketFail:
			failedRuns = append(failedRuns, fmt.Sprintf("%s (%s, %s)", run.Name, run.Conclusion, run.URL))
		}
	}

	// A run that failed while every check it reported passed - typically one
	// that never started a job, as with a workflow file GitHub rejected or an
	// Actions outage - is not something a fix session can be pointed at.
	if len(failedRuns) > 0 {
		return gh.ChecksPending, fmt.Errorf("%w: workflow run(s) of %s failed without a failing check: %s",
			ErrCIUnconfirmed, sha, strings.Join(failedRuns, "; "))
	}
	if len(waiting) > 0 {
		return gh.ChecksPending, nil
	}
	if len(missing) > 0 {
		if inWindow {
			r.announceCIRegistration(cw, "waiting for required check(s) "+strings.Join(missing, ", ")+" to report")
			return gh.ChecksPending, nil
		}
		return gh.ChecksPending, fmt.Errorf("%w: required check(s) %s of %s did not report for %s after %s",
			ErrCIUnconfirmed, strings.Join(missing, ", "), r.pr.BaseRefName, sha, r.timing.noChecksWindow())
	}
	if len(runs) == 0 && inWindow && r.workflowsExpected(cw) {
		r.announceCIRegistration(cw, "waiting for GitHub Actions to register the runs of "+sha)
		return gh.ChecksPending, nil
	}
	if len(checks) == 0 && len(runs) == 0 {
		if inWindow {
			return gh.ChecksPending, nil
		}
		return gh.ChecksNone, nil
	}
	return gh.ChecksPass, nil
}

// workflowsExpected reports whether the repository has workflow files that
// could start a run for the head. A failed lookup counts as yes: waiting out
// the window is cheaper than a green that was never confirmed.
func (r *run) workflowsExpected(cw *ciWait) bool {
	if !cw.hasWorkflowsSet {
		has, err := r.p.GH.HasActiveWorkflows(r.ctx, r.o.Repo)
		cw.hasWorkflows = has || err != nil
		cw.hasWorkflowsSet = err == nil
		return cw.hasWorkflows
	}
	return cw.hasWorkflows
}

func (r *run) announceCIRegistration(cw *ciWait, msg string) {
	if cw.announced {
		return
	}
	cw.announced = true
	r.rep.Info(msg)
}

func (r *run) sleep(d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-r.ctx.Done():
		return r.ctx.Err()
	}
}

func writeTail(path, text string, n int) {
	lines := splitLines(text)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	os.WriteFile(path, []byte(joinLines(lines)), 0o644)
}

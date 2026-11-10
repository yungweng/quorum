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
}

type bgResult struct {
	res *review.Result
	err error
}

// startReview kicks off a review in the background.
//
// A review only needs the pushed PR head, not green checks, so it overlaps the
// CI wait instead of queueing behind it. It must never overlap a Codex fix
// session: a fix moves the head sha out from under the findings, which is why
// ensureCIGreen cancels a running review before it starts one.
func (r *run) startReview(round int) { r.startReviewWith(round, "") }

// startReviewWith starts a review in the background, reusing resumeDir's
// completed reviewer output when it is non-empty.
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
		br.done <- bgResult{res, err}
	}()
}

func (r *run) reviewOptions() review.Options {
	o := review.Options{
		Repo:             r.o.Repo,
		Number:           r.pr.Number,
		Branch:           branchOnlyValue(r.target.BranchOnly, r.branch),
		HeadSHA:          branchOnlyValue(r.target.BranchOnly, r.headSHA),
		RepoRoot:         r.o.RepoRoot,
		Runs:             r.o.Reviewers,
		Engine:           r.o.ReviewEngine,
		Model:            r.o.ReviewModel,
		Effort:           r.o.ReviewEffort,
		Rules:            r.o.Rules,
		BaseBranch:       r.pr.BaseRefName,
		Post:             r.o.Post && !r.target.BranchOnly,
		UseDirenv:        r.o.UseDirenv,
		AllowEnvrcChange: r.o.AllowEnvrcChange,
		RunsDir:          r.o.ReviewRunsDir,
		SharedRunsDirs:   []string{r.o.RunsDir},
		DepsDir:          r.o.DepsDir,
		CodexBin:         r.o.CodexBin,
		ClaudeBin:        r.o.ClaudeBin,
		GrokBin:          r.o.GrokBin,
		DirenvBin:        r.o.DirenvBin,
		ReviewTimeout:    review.DefaultReviewTimeout,
	}
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
		r.startReview(round)
	}
	br := r.review
	label := fmt.Sprintf("Review round %d", round)
	r.rep.StepStart(label, r.reviewModel)

	out := r.waitReview(br, label)
	r.review = nil
	if out.err != nil {
		if br.resume != "" && resumeFallbackEligible(out.err) {
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

// resumeFallbackEligible reports whether a resumed round failed for a
// resume-specific reason - the directory turned out unusable or held too
// little reviewer output - rather than the round itself being the problem.
func resumeFallbackEligible(err error) bool {
	return errors.Is(err, review.ErrResumeUnusable) || errors.Is(err, review.ErrTooFewReviewers)
}

// resumableRunDir reports whether err is a round failure whose completed
// reviewer output is worth resuming, and where it lives.
func resumableRunDir(err error) (string, bool) {
	if !errors.Is(err, review.ErrAggregatorInvalid) &&
		!errors.Is(err, review.ErrVerifierInvalid) &&
		!errors.Is(err, usagelimit.Err) {
		return "", false
	}
	var rd *review.RunDirError
	if !errors.As(err, &rd) || rd.RunDir == "" {
		return "", false
	}
	return rd.RunDir, true
}

// waitReview blocks on a background review while the status line ticks.
func (r *run) waitReview(br *bgReview, label string) bgResult {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case out := <-br.done:
			r.rep.StepEnd(label, r.reviewModel, time.Since(br.started), out.err == nil)
			return out
		case <-ticker.C:
			r.rep.StepTick(label, r.reviewModel, time.Since(br.started))
		case <-r.ctx.Done():
			br.cancel()
			out := <-br.done
			return out
		}
	}
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

// ensureCIGreen waits for CI and repairs it, up to MaxCIFixes attempts.
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
		tag := fmt.Sprintf("ci-fix-%d", fixNumber)
		r.enter(PhaseCIFix)
		if err := r.codexCall(tag, ciFixPrompt(r.pr.Number, string(failsJSON))); err != nil {
			return err
		}
		if err := r.questionGate(tag); err != nil {
			return err
		}
		if err := r.ensureCommitted(tag); err != nil {
			return err
		}
		r.ciFixTotal = fixNumber
		label := fmt.Sprintf("CI fix %d", fixNumber)
		r.recordRound(label, preSHA)
		if err := r.pushBranchWithFixes(); err != nil {
			return err
		}
		if r.o.DivergenceScan {
			r.traceCIFix(preSHA, r.headSHA, tag)
		}
		if err := r.postFixComment(tag, label, "", "", preSHA); err != nil {
			return err
		}
	}
}

// awaitChecks runs watchCI in the background so the reporter can tick the CI
// wait the way waitReview ticks a review: `gh pr checks --watch` blocks for as
// long as CI runs, so the elapsed time cannot come from inside the poll loop.
// Cancellation needs no extra case here: watchCI's own ctx handling makes it
// return, which closes the wait through the same channel.
func (r *run) awaitChecks(started time.Time) (gh.CheckState, error) {
	r.rep.CIWait(r.pr.Number, 0)
	type checksResult struct {
		state gh.CheckState
		err   error
	}
	done := make(chan checksResult, 1)
	go func() {
		state, err := r.watchCI()
		done <- checksResult{state, err}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case out := <-done:
			return out.state, out.err
		case <-ticker.C:
			r.rep.CIWait(r.pr.Number, time.Since(started))
		}
	}
}

// Poll intervals of the CI wait. Variables so tests can shorten them.
var (
	ciPendingPoll  = 10 * time.Second
	ciNoChecksPoll = 20 * time.Second
)

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
			if err := r.sleep(ciPendingPoll); err != nil {
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
			poll := ciPendingPoll
			if state == gh.ChecksNone {
				poll = ciNoChecksPoll
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
	inWindow := time.Since(cw.started) < noChecksTimeout

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
			ErrCIUnconfirmed, strings.Join(missing, ", "), r.pr.BaseRefName, sha, noChecksTimeout)
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
	if cw.announced || r.rep == nil {
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

// writeTail keeps the last n lines of gh's output for inspection.
func writeTail(path, text string, n int) {
	lines := splitLines(text)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	os.WriteFile(path, []byte(joinLines(lines)), 0o644)
}

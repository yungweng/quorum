package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/review"
)

// bgReview is a review running while the pipeline waits for CI.
type bgReview struct {
	round   int
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
// ensureCIGreen discards a running review before it starts one.
func (r *run) startReview(round int) {
	if r.review != nil {
		return
	}
	ctx, cancel := context.WithCancel(r.ctx)
	br := &bgReview{
		round:   round,
		started: time.Now(),
		cancel:  cancel,
		done:    make(chan bgResult, 1),
	}
	r.review = br

	opts := r.reviewOptions()
	go func() {
		res, err := r.p.Review.Run(ctx, opts)
		br.done <- bgResult{res, err}
	}()
}

func (r *run) reviewOptions() review.Options {
	return review.Options{
		Repo:             r.o.Repo,
		Number:           r.pr.Number,
		Branch:           branchOnlyValue(r.target.BranchOnly, r.branch),
		HeadSHA:          branchOnlyValue(r.target.BranchOnly, r.headSHA),
		RepoRoot:         r.o.RepoRoot,
		Runs:             r.o.Reviewers,
		Model:            r.o.ReviewModel,
		Effort:           r.o.ReviewEffort,
		BaseBranch:       r.pr.BaseRefName,
		Post:             !r.target.BranchOnly,
		UseDirenv:        r.o.UseDirenv,
		AllowEnvrcChange: r.o.AllowEnvrcChange,
		RunsDir:          r.o.ReviewRunsDir,
		SharedRunsDirs:   []string{r.o.RunsDir},
		DepsDir:          r.o.DepsDir,
		CodexBin:         r.o.CodexBin,
		DirenvBin:        r.o.DirenvBin,
		ReviewTimeout:    review.DefaultReviewTimeout,
	}
}

func branchOnlyValue(branchOnly bool, branch string) string {
	if branchOnly {
		return branch
	}
	return ""
}

// finishReview joins the running review and returns its findings plus the
// comment text that goes into the next fix round.
func (r *run) finishReview(round int) (review.Findings, string, error) {
	if r.review == nil {
		r.startReview(round)
	}
	br := r.review
	label := fmt.Sprintf("Review round %d", round)
	r.rep.StepStart(label)

	out := r.waitReview(br, label)
	r.review = nil
	if out.err != nil {
		return review.Findings{}, "", fmt.Errorf("review round %d failed: %w", round, out.err)
	}

	comment, err := os.ReadFile(out.res.CommentFile)
	if err != nil {
		return review.Findings{}, "", fmt.Errorf("reading the review comment: %w", err)
	}
	return out.res.Findings, string(comment), nil
}

// waitReview blocks on a background review while the status line ticks.
func (r *run) waitReview(br *bgReview, label string) bgResult {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case out := <-br.done:
			r.rep.StepEnd(label, time.Since(br.started), out.err == nil)
			return out
		case <-ticker.C:
			r.rep.StepTick(label, time.Since(br.started))
		case <-r.ctx.Done():
			br.cancel()
			out := <-br.done
			return out
		}
	}
}

// discardReview waits out a review whose findings a CI fix is about to
// invalidate, and drops its result.
//
// Waiting rather than cancelling is deliberate. The review of the outgoing head
// still finishes and posts its comment; only the pipeline stops acting on it.
// Cancelling would save the remaining reviewer passes, but it also throws away a
// review that is already paid for and that still describes the code as it stood
// a moment ago. The fix session runs afterwards either way.
func (r *run) discardReview() {
	br := r.review
	if br == nil {
		return
	}
	r.review = nil
	r.rep.Info("CI needs a fix; discarding the review of the outgoing head...")
	r.waitReview(br, fmt.Sprintf("Review round %d (discarded)", br.round))
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
		r.rep.Info(fmt.Sprintf("waiting for CI on PR #%d...", r.pr.Number))
		r.prog.CI = CIWaiting
		r.enter(PhaseCI)
		state, err := r.watchCI()
		if err != nil {
			return err
		}
		switch state {
		case gh.ChecksPass:
			r.rep.CIGreen()
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
		if err := r.pushBranch(); err != nil {
			return err
		}
		r.postFixComment(tag, label, "", "", preSHA)
	}
}

// watchCI blocks until the checks settle.
//
// GitHub does not register checks the instant a commit lands, so a PR that
// really has CI can briefly look like it has none. Only after noChecksTimeout
// of asking is "no checks" accepted as the answer.
func (r *run) watchCI() (gh.CheckState, error) {
	deadline := time.Now().Add(noChecksTimeout)
	for {
		state, out, err := r.p.GH.WatchChecks(r.ctx, r.o.RepoRoot, r.pr.Number)
		if err != nil {
			return state, err
		}
		writeTail(filepath.Join(r.logDir, "ci-last.log"), out, 20)

		switch state {
		case gh.ChecksNone:
			if time.Now().After(deadline) {
				return gh.ChecksNone, nil
			}
			if err := r.sleep(20 * time.Second); err != nil {
				return state, err
			}
		case gh.ChecksPending:
			if err := r.sleep(10 * time.Second); err != nil {
				return state, err
			}
		default:
			return state, nil
		}
	}
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

// Package automerge approves and merges one reviewed pull request head.
package automerge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/review"
)

const approvalText = "No blockers or critical findings found."
const approvalProvenance = "<!-- quorum:auto-merge-approval:v1 -->"
const approvalBody = approvalText + " " + approvalProvenance
const driftDismissalBody = "The pull request head changed after this approval was submitted."
const failureDismissalBody = "Automatic merge did not complete, so this approval is no longer active."
const approvalCleanupTimeout = 30 * time.Second

const (
	Merged           = "merged"
	ApprovalRequired = "awaiting approval"
)

// ErrMergeNotReady means GitHub refused the exact-head merge because branch
// requirements have not settled yet. Callers may wait for checks and retry.
var ErrMergeNotReady = errors.New("reviewed head is not ready to merge")
var errHeadDrift = errors.New("pull request head moved after review")

type Result struct {
	ApprovalAttempted bool
	ApprovalCreated   bool
	Status            string
	approvalReviewID  int64
}

// Eligible is deliberately the same threshold as loop convergence. Questions
// and suggestions do not block; an unposted or branch-only review does.
func Eligible(findings review.Findings) bool {
	return findings.PR > 0 && findings.Posted && findings.Blocking() == 0
}

// Allowed also requires the source-specific option and global posting policy.
// Fix loops post their own reviews, so Findings.Posted alone cannot represent
// POST=0 at these call sites.
func Allowed(enabled, post bool, findings review.Findings) bool {
	return enabled && post && Eligible(findings)
}

// Run binds both side effects to reviewedSHA. It is safe to repeat: an
// existing approval for that commit is reused, and a merged PR is success.
func Run(ctx context.Context, client *gh.Client, repo string, number int, reviewedSHA string) (Result, error) {
	var result Result
	mergeMethod := gh.MergeMethodMerge
	if repo == "" || number <= 0 || reviewedSHA == "" {
		return result, fmt.Errorf("auto-merge needs a repository, pull request number, and reviewed head sha")
	}

	pr, err := client.PRDetails(ctx, repo, number)
	if err != nil {
		return result, err
	}
	if merged, err := validateHead(pr, repo, number, reviewedSHA); merged || err != nil {
		if merged {
			result.Status = Merged
		}
		return result, err
	}
	login, err := client.Login(ctx)
	if err != nil {
		return result, err
	}
	if strings.EqualFold(pr.Author.Login, login) {
		result.Status = ApprovalRequired
		return result, nil
	}
	if pr.BaseRefName != "" {
		settings, err := client.MergePolicy(ctx, repo, number, reviewedSHA)
		if err != nil {
			return result, fmt.Errorf("checking merge policy: %w", err)
		}
		if settings.QueueEnabled {
			return result, fmt.Errorf("refusing auto-merge: target branch %s requires a merge queue", pr.BaseRefName)
		}
		var ok bool
		mergeMethod, ok = preferredMergeMethod(settings)
		if !ok {
			return result, fmt.Errorf("refusing auto-merge: repository %s does not allow a supported merge method", repo)
		}
	}

	reviews, err := client.Reviews(ctx, repo, number)
	if err != nil {
		return result, err
	}
	knownReviewIDs := make(map[int64]struct{}, len(reviews))
	for _, existing := range reviews {
		knownReviewIDs[existing.ID] = struct{}{}
	}
	// Derive current reviewer state from submission time, independent of the
	// order in which GitHub or a paginated response happens to return reviews.
	// Review IDs break ties when GitHub timestamps have the same precision.
	sort.SliceStable(reviews, func(i, j int) bool {
		if reviews[i].SubmittedAt.Equal(reviews[j].SubmittedAt) {
			return reviews[i].ID < reviews[j].ID
		}
		return reviews[i].SubmittedAt.Before(reviews[j].SubmittedAt)
	})
	approved := false
	activeChangeRequests := make(map[string]bool)
	var reusableApprovalID int64
	var createdReview gh.PRReview
	for _, review := range reviews {
		if !strings.EqualFold(review.User.Login, login) {
			reviewer := strings.ToLower(review.User.Login)
			switch review.State {
			case "CHANGES_REQUESTED":
				activeChangeRequests[reviewer] = true
			case "APPROVED", "DISMISSED":
				delete(activeChangeRequests, reviewer)
			}
			continue
		}
		switch review.State {
		case "APPROVED":
			approved = review.CommitID == reviewedSHA
			reusableApprovalID = 0
			if approved && review.Body == approvalBody {
				reusableApprovalID = review.ID
			}
		case "CHANGES_REQUESTED", "DISMISSED":
			approved = false
			reusableApprovalID = 0
		}
	}
	if approved {
		result.approvalReviewID = reusableApprovalID
	}
	if len(activeChangeRequests) > 0 {
		return result, dismissCreatedApprovalAfterFailure(ctx, client, repo, number, &result,
			fmt.Errorf("refusing auto-merge: pull request %s#%d has active change requests", repo, number))
	}
	if !approved {
		current, err := client.PRDetails(ctx, repo, number)
		if err != nil {
			return result, err
		}
		if merged, err := validateHead(current, repo, number, reviewedSHA); merged || err != nil {
			if merged {
				result.Status = Merged
			}
			return result, err
		}
		result.ApprovalAttempted = true
		createdReview, err = client.ApproveHead(ctx, repo, number, reviewedSHA, approvalBody)
		if err != nil {
			approvalErr := fmt.Errorf("approving reviewed head: %w", err)
			if errors.Is(err, gh.ErrTransient) {
				approvalErr = reconcileApprovalFailure(ctx, client, repo, number, login, reviewedSHA, knownReviewIDs, &result, approvalErr)
			}
			return result, approvalErr
		}
		result.ApprovalCreated = true
		result.approvalReviewID = createdReview.ID
	}

	// Re-read the head after approval, then use GitHub's head-bound merge API.
	current, err := client.PRDetails(ctx, repo, number)
	if err != nil {
		return result, dismissCreatedApprovalAfterFailure(ctx, client, repo, number, &result, err)
	}
	if merged, err := validateHead(current, repo, number, reviewedSHA); merged || err != nil {
		if merged {
			result.Status = Merged
		}
		if err != nil {
			if current.HeadRefOid != reviewedSHA {
				err = dismissCreatedApproval(ctx, client, repo, number, &result, err)
			} else {
				err = dismissCreatedApprovalAfterFailure(ctx, client, repo, number, &result, err)
			}
		}
		return result, err
	}
	if latestReviewsRequestChanges(current.LatestReviews, login) {
		return result, dismissCreatedApprovalAfterFailure(ctx, client, repo, number, &result,
			fmt.Errorf("refusing auto-merge: pull request %s#%d has active change requests", repo, number))
	}
	if mergeErr := client.MergeHead(ctx, repo, number, reviewedSHA, mergeMethod); mergeErr != nil {
		current, inspectErr := client.PRDetails(ctx, repo, number)
		if inspectErr != nil {
			cause := errors.Join(fmt.Errorf("merging reviewed head: %w", mergeErr),
				fmt.Errorf("rechecking pull request after merge failure: %w", inspectErr))
			return result, dismissCreatedApprovalAfterFailure(ctx, client, repo, number, &result, cause)
		}
		merged, headErr := validateHead(current, repo, number, reviewedSHA)
		if headErr != nil {
			if current.HeadRefOid != reviewedSHA {
				headErr = dismissCreatedApproval(ctx, client, repo, number, &result, headErr)
			} else {
				headErr = dismissCreatedApprovalAfterFailure(ctx, client, repo, number, &result, headErr)
			}
			return result, headErr
		}
		if merged {
			result.Status = Merged
			return result, nil
		}
		if mergeReadinessPending(current, mergeErr) {
			return result, fmt.Errorf("%w: %v", ErrMergeNotReady, mergeErr)
		}
		return result, dismissCreatedApprovalAfterFailure(ctx, client, repo, number, &result,
			fmt.Errorf("merging reviewed head: %w", mergeErr))
	}
	result.Status = Merged
	return result, nil
}

// preferredMergeMethod preserves merge commits where they already work, then
// falls back to the other methods GitHub exposes. GitHub repositories record
// which methods are allowed, but do not expose a preferred method.
func preferredMergeMethod(settings gh.MergeSettings) (gh.MergeMethod, bool) {
	switch {
	case settings.MergeCommitAllowed:
		return gh.MergeMethodMerge, true
	case settings.SquashMergeAllowed:
		return gh.MergeMethodSquash, true
	case settings.RebaseMergeAllowed:
		return gh.MergeMethodRebase, true
	default:
		return "", false
	}
}

func mergeReadinessPending(current gh.Details, mergeErr error) bool {
	message := strings.ToLower(mergeErr.Error())
	if !strings.Contains(message, "http 405") || !strings.Contains(message, "pull request is not mergeable") {
		return false
	}
	return current.Mergeable == "MERGEABLE" && current.MergeStateStatus == "BLOCKED" ||
		current.Mergeable == "UNKNOWN" && current.MergeStateStatus == "UNKNOWN"
}

func latestReviewsRequestChanges(reviews []gh.LatestReview, login string) bool {
	for _, review := range reviews {
		if review.State == "CHANGES_REQUESTED" && !strings.EqualFold(review.Author.Login, login) {
			return true
		}
	}
	return false
}

// RetryWhenReady waits for checks, then retries the exact-head merge. A green
// check result can precede GitHub's mergeability update, so ErrMergeNotReady
// remains retryable until waitTimeout expires. A zero timeout waits until the
// caller cancels the context.
func RetryWhenReady(ctx context.Context, client *gh.Client, checksDir, repo string, number int, reviewedSHA string, initial Result, waitTimeout time.Duration) (Result, error) {
	var deadline time.Time
	if waitTimeout > 0 {
		deadline = time.Now().Add(waitTimeout)
	}
	combined := initial
	for {
		if done, err := verifyRetryHead(ctx, client, repo, number, reviewedSHA, &combined); done || err != nil {
			return combined, err
		}
		watchCtx := ctx
		cancel := func() {}
		if !deadline.IsZero() {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return stopRetry(ctx, client, repo, number, combined,
					fmt.Errorf("required checks did not settle within %s", waitTimeout))
			}
			watchCtx, cancel = context.WithTimeout(ctx, remaining)
		}
		checkState, output, err := client.WatchRequiredChecks(watchCtx, checksDir, number)
		watchErr := watchCtx.Err()
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return stopRetry(ctx, client, repo, number, combined, ctx.Err())
			}
			if errors.Is(watchErr, context.DeadlineExceeded) {
				return stopRetry(ctx, client, repo, number, combined,
					fmt.Errorf("required checks did not settle within %s", waitTimeout))
			}
			if errors.Is(err, gh.ErrTransient) {
				if deadlineExpired(deadline) {
					return stopRetry(ctx, client, repo, number, combined,
						fmt.Errorf("required checks did not settle within %s", waitTimeout))
				}
				delay := retryDelay(time.Second, waitTimeout)
				if !deadline.IsZero() {
					delay = min(delay, time.Until(deadline))
				}
				if err := waitRetry(ctx, delay); err != nil {
					return stopRetry(ctx, client, repo, number, combined, err)
				}
				continue
			}
			return stopRetry(ctx, client, repo, number, combined,
				fmt.Errorf("waiting for required checks: %w", err))
		}
		if done, err := verifyRetryHead(ctx, client, repo, number, reviewedSHA, &combined); done || err != nil {
			return combined, err
		}

		var delay time.Duration
		switch checkState {
		case gh.ChecksPass:
			result, mergeErr := Run(ctx, client, repo, number, reviewedSHA)
			combined = combineResults(combined, result)
			if mergeErr == nil {
				return combined, nil
			}
			if !errors.Is(mergeErr, ErrMergeNotReady) {
				return stopRetry(ctx, client, repo, number, combined, mergeErr)
			}
			if deadlineExpired(deadline) {
				return stopRetry(ctx, client, repo, number, combined, mergeErr)
			}
			delay = retryDelay(5*time.Second, waitTimeout)
		case gh.ChecksFail:
			return stopRetry(ctx, client, repo, number, combined,
				fmt.Errorf("required checks failed: %s", firstLine(output)))
		case gh.ChecksNone:
			result, mergeErr := Run(ctx, client, repo, number, reviewedSHA)
			combined = combineResults(combined, result)
			if mergeErr == nil {
				return combined, nil
			}
			if !errors.Is(mergeErr, ErrMergeNotReady) || deadlineExpired(deadline) {
				return stopRetry(ctx, client, repo, number, combined, mergeErr)
			}
			delay = retryDelay(20*time.Second, waitTimeout)
		case gh.ChecksPending:
			if deadlineExpired(deadline) {
				return stopRetry(ctx, client, repo, number, combined,
					fmt.Errorf("required checks did not settle within %s", waitTimeout))
			}
			delay = retryDelay(10*time.Second, waitTimeout)
		}

		if !deadline.IsZero() {
			delay = min(delay, time.Until(deadline))
		}
		if err := waitRetry(ctx, delay); err != nil {
			return stopRetry(ctx, client, repo, number, combined, err)
		}
	}
}

func retryDelay(limit, waitTimeout time.Duration) time.Duration {
	if waitTimeout <= 0 {
		return limit
	}
	return min(limit, max(waitTimeout/4, time.Millisecond))
}

func deadlineExpired(deadline time.Time) bool {
	return !deadline.IsZero() && !time.Now().Before(deadline)
}

func combineResults(first, second Result) Result {
	reviewID := first.approvalReviewID
	if second.approvalReviewID != 0 {
		reviewID = second.approvalReviewID
	}
	return Result{
		ApprovalAttempted: first.ApprovalAttempted || second.ApprovalAttempted,
		ApprovalCreated:   first.ApprovalCreated || second.ApprovalCreated,
		Status:            second.Status,
		approvalReviewID:  reviewID,
	}
}

func stopRetry(ctx context.Context, client *gh.Client, repo string, number int, result Result, cause error) (Result, error) {
	err := dismissCreatedApprovalAfterFailure(ctx, client, repo, number, &result, cause)
	return result, err
}

func reconcileApprovalFailure(ctx context.Context, client *gh.Client, repo string, number int, login, reviewedSHA string, knownReviewIDs map[int64]struct{}, result *Result, approvalErr error) error {
	reviews, err := client.Reviews(ctx, repo, number)
	if err != nil {
		return errors.Join(approvalErr, fmt.Errorf("reconciling timed-out approval: %w", err))
	}
	for _, candidate := range reviews {
		_, existed := knownReviewIDs[candidate.ID]
		if candidate.ID != 0 && !existed && candidate.State == "APPROVED" &&
			candidate.CommitID == reviewedSHA && candidate.Body == approvalBody &&
			strings.EqualFold(candidate.User.Login, login) {
			result.ApprovalCreated = true
			result.approvalReviewID = candidate.ID
			break
		}
	}

	current, err := client.PRDetails(ctx, repo, number)
	if err != nil {
		return dismissCreatedApprovalAfterFailure(ctx, client, repo, number, result,
			errors.Join(approvalErr, fmt.Errorf("reconciling timed-out approval: %w", err)))
	}
	merged, headErr := validateHead(current, repo, number, reviewedSHA)
	if errors.Is(headErr, errHeadDrift) {
		return dismissCreatedApproval(ctx, client, repo, number, result, errors.Join(approvalErr, headErr))
	}
	if headErr != nil {
		return dismissCreatedApprovalAfterFailure(ctx, client, repo, number, result, errors.Join(approvalErr, headErr))
	}
	if merged {
		result.Status = Merged
		return nil
	}
	return dismissCreatedApprovalAfterFailure(ctx, client, repo, number, result, approvalErr)
}

func waitRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	select {
	case <-ctx.Done():
		timer.Stop()
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func validateHead(current gh.Details, repo string, number int, reviewedSHA string) (bool, error) {
	if current.IsCrossRepository {
		return false, fmt.Errorf("refusing auto-merge for fork pull request %s#%d", repo, number)
	}
	if current.HeadRefOid != reviewedSHA {
		return false, fmt.Errorf("%w: refusing auto-merge: reviewed head is %s but GitHub reports %s", errHeadDrift, reviewedSHA, current.HeadRefOid)
	}
	if current.State == gh.StateMerged {
		return true, nil
	}
	if current.State != gh.StateOpen {
		return false, fmt.Errorf("pull request %s#%d is %s, not open", repo, number, strings.ToLower(current.State))
	}
	if current.AutoMergeRequest != nil {
		return false, fmt.Errorf("refusing auto-merge: pull request %s#%d already has an auto-merge request", repo, number)
	}
	if current.IsDraft {
		return false, fmt.Errorf("refusing auto-merge: pull request %s#%d is a draft", repo, number)
	}
	if current.ReviewDecision == "CHANGES_REQUESTED" {
		return false, fmt.Errorf("refusing auto-merge: pull request %s#%d has active change requests", repo, number)
	}
	return false, nil
}

func verifyRetryHead(ctx context.Context, client *gh.Client, repo string, number int, reviewedSHA string, result *Result) (bool, error) {
	current, err := client.PRDetails(ctx, repo, number)
	if err != nil {
		return false, dismissCreatedApprovalAfterFailure(ctx, client, repo, number, result, err)
	}
	merged, err := validateHead(current, repo, number, reviewedSHA)
	if err != nil {
		if errors.Is(err, errHeadDrift) {
			err = dismissCreatedApproval(ctx, client, repo, number, result, err)
		} else {
			err = dismissCreatedApprovalAfterFailure(ctx, client, repo, number, result, err)
		}
		return false, err
	}
	if merged {
		result.Status = Merged
		return true, nil
	}
	return false, nil
}

func dismissCreatedApproval(ctx context.Context, client *gh.Client, repo string, number int, result *Result, cause error) error {
	return dismissCreatedApprovalWith(ctx, client, repo, number, result, driftDismissalBody, "after head drift", cause)
}

func dismissCreatedApprovalAfterFailure(ctx context.Context, client *gh.Client, repo string, number int, result *Result, cause error) error {
	return dismissCreatedApprovalWith(ctx, client, repo, number, result, failureDismissalBody, "after auto-merge stopped", cause)
}

func dismissCreatedApprovalWith(ctx context.Context, client *gh.Client, repo string, number int, result *Result, message, reason string, cause error) error {
	if result.approvalReviewID == 0 {
		return cause
	}
	reviewID := result.approvalReviewID
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), approvalCleanupTimeout)
	defer cancel()
	if err := client.DismissReview(cleanupCtx, repo, number, reviewID, message); err != nil {
		return errors.Join(cause, fmt.Errorf("dismissing approval %d %s: %w", reviewID, reason, err))
	}
	result.approvalReviewID = 0
	return cause
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if line, _, ok := strings.Cut(text, "\n"); ok {
		return line
	}
	if text == "" {
		return "no output"
	}
	return text
}

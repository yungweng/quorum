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

const approvalBody = "No blockers or critical findings found."
const driftDismissalBody = "The pull request head changed after this approval was submitted."

const (
	Merged = "merged"
)

// ErrMergeNotReady means GitHub refused the exact-head merge because branch
// requirements have not settled yet. Callers may wait for checks and retry.
var ErrMergeNotReady = errors.New("reviewed head is not ready to merge")
var errHeadDrift = errors.New("pull request head moved after review")

// WaitTimeout bounds check registration and GitHub's mergeability lag.
const WaitTimeout = 180 * time.Second

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
		return result, fmt.Errorf("cannot approve your own pull request %s#%d", repo, number)
	}

	reviews, err := client.Reviews(ctx, repo, number)
	if err != nil {
		return result, err
	}
	// Derive current reviewer state from submission time, independent of the
	// order in which GitHub or a paginated response happens to return reviews.
	sort.SliceStable(reviews, func(i, j int) bool {
		return reviews[i].SubmittedAt.Before(reviews[j].SubmittedAt)
	})
	approved := false
	var createdReview gh.PRReview
	for _, review := range reviews {
		if !strings.EqualFold(review.User.Login, login) {
			continue
		}
		switch review.State {
		case "APPROVED":
			approved = review.CommitID == reviewedSHA
		case "CHANGES_REQUESTED", "DISMISSED":
			approved = false
		}
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
			return result, fmt.Errorf("approving reviewed head: %w", err)
		}
		result.ApprovalCreated = true
		result.approvalReviewID = createdReview.ID
	}

	// Re-read the head after approval, then use GitHub's head-bound merge API.
	current, err := client.PRDetails(ctx, repo, number)
	if err != nil {
		return result, dismissCreatedApproval(ctx, client, repo, number, &result, err)
	}
	if merged, err := validateHead(current, repo, number, reviewedSHA); merged || err != nil {
		if merged {
			result.Status = Merged
		}
		if err != nil && current.HeadRefOid != reviewedSHA {
			err = dismissCreatedApproval(ctx, client, repo, number, &result, err)
		}
		return result, err
	}
	if err := client.MergeHead(ctx, repo, number, reviewedSHA); err != nil {
		if current, inspectErr := client.PRDetails(ctx, repo, number); inspectErr == nil {
			merged, headErr := validateHead(current, repo, number, reviewedSHA)
			if headErr != nil {
				if current.HeadRefOid != reviewedSHA {
					headErr = dismissCreatedApproval(ctx, client, repo, number, &result, headErr)
				}
				return result, headErr
			}
			if merged {
				result.Status = Merged
				return result, nil
			}
		}
		// GitHub uses 405 while an otherwise valid pull request is waiting
		// for protected-branch requirements. Keep other errors terminal.
		if strings.Contains(err.Error(), "HTTP 405") {
			return result, fmt.Errorf("%w: %v", ErrMergeNotReady, err)
		}
		return result, fmt.Errorf("merging reviewed head: %w", err)
	}
	result.Status = Merged
	return result, nil
}

// RetryWhenReady waits for checks, then retries the exact-head merge. A green
// check result can precede GitHub's mergeability update, so ErrMergeNotReady
// remains retryable until waitTimeout expires.
func RetryWhenReady(ctx context.Context, client *gh.Client, checksDir, repo string, number int, reviewedSHA string, initial Result, waitTimeout time.Duration) (Result, error) {
	deadline := time.Now().Add(waitTimeout)
	combined := initial
	for {
		if done, err := verifyRetryHead(ctx, client, repo, number, reviewedSHA, &combined); done || err != nil {
			return combined, err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return combined, fmt.Errorf("required checks did not settle within %s", waitTimeout)
		}
		watchCtx, cancel := context.WithTimeout(ctx, remaining)
		checkState, output, err := client.WatchChecks(watchCtx, checksDir, number)
		watchErr := watchCtx.Err()
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return combined, ctx.Err()
			}
			if errors.Is(watchErr, context.DeadlineExceeded) {
				return combined, fmt.Errorf("required checks did not settle within %s", waitTimeout)
			}
			return combined, fmt.Errorf("waiting for required checks: %w", err)
		}
		if done, err := verifyRetryHead(ctx, client, repo, number, reviewedSHA, &combined); done || err != nil {
			return combined, err
		}

		var delay time.Duration
		switch checkState {
		case gh.ChecksPass:
			result, mergeErr := Run(ctx, client, repo, number, reviewedSHA)
			combined = combineResults(combined, result)
			if !errors.Is(mergeErr, ErrMergeNotReady) {
				if errors.Is(mergeErr, errHeadDrift) {
					mergeErr = dismissCreatedApproval(ctx, client, repo, number, &combined, mergeErr)
				}
				return combined, mergeErr
			}
			if !time.Now().Before(deadline) {
				return combined, mergeErr
			}
			delay = retryDelay(5*time.Second, waitTimeout)
		case gh.ChecksFail:
			return combined, fmt.Errorf("required checks failed: %s", firstLine(output))
		case gh.ChecksNone:
			if !time.Now().Before(deadline) {
				result, mergeErr := Run(ctx, client, repo, number, reviewedSHA)
				combined = combineResults(combined, result)
				if errors.Is(mergeErr, errHeadDrift) {
					mergeErr = dismissCreatedApproval(ctx, client, repo, number, &combined, mergeErr)
				}
				return combined, mergeErr
			}
			delay = retryDelay(20*time.Second, waitTimeout)
		case gh.ChecksPending:
			if !time.Now().Before(deadline) {
				return combined, fmt.Errorf("required checks did not settle within %s", waitTimeout)
			}
			delay = retryDelay(10*time.Second, waitTimeout)
		}

		delay = min(delay, time.Until(deadline))
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return combined, ctx.Err()
		case <-timer.C:
		}
	}
}

func retryDelay(limit, waitTimeout time.Duration) time.Duration {
	return min(limit, max(waitTimeout/4, time.Millisecond))
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

func validateHead(current gh.Details, repo string, number int, reviewedSHA string) (bool, error) {
	if current.HeadRefOid != reviewedSHA {
		return false, fmt.Errorf("%w: refusing auto-merge: reviewed head is %s but GitHub reports %s", errHeadDrift, reviewedSHA, current.HeadRefOid)
	}
	if current.State == gh.StateMerged {
		return true, nil
	}
	if current.State != gh.StateOpen {
		return false, fmt.Errorf("pull request %s#%d is %s, not open", repo, number, strings.ToLower(current.State))
	}
	return false, nil
}

func verifyRetryHead(ctx context.Context, client *gh.Client, repo string, number int, reviewedSHA string, result *Result) (bool, error) {
	current, err := client.PRDetails(ctx, repo, number)
	if err != nil {
		return false, dismissCreatedApproval(ctx, client, repo, number, result, err)
	}
	merged, err := validateHead(current, repo, number, reviewedSHA)
	if err != nil {
		if errors.Is(err, errHeadDrift) {
			err = dismissCreatedApproval(ctx, client, repo, number, result, err)
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
	if result.approvalReviewID == 0 {
		return cause
	}
	reviewID := result.approvalReviewID
	if err := client.DismissReview(ctx, repo, number, reviewID, driftDismissalBody); err != nil {
		return errors.Join(cause, fmt.Errorf("dismissing approval %d after head drift: %w", reviewID, err))
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

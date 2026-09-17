package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/yungweng/quorum/internal/automerge"
	"github.com/yungweng/quorum/internal/gh"
)

func (a *app) autoMerge(ctx context.Context, client *gh.Client, checksDir, repo string, number int, sha string) (automerge.Result, error) {
	result, err := automerge.Run(ctx, client, repo, number, sha, a.cfg.AutoMergeAuthors)
	if errors.Is(err, automerge.ErrMergeNotReady) {
		retryResult, retryErr := automerge.RetryWhenReady(
			ctx, client, checksDir, repo, number, sha, a.cfg.AutoMergeAuthors, result, a.cfg.AutoMergeTimeout,
		)
		result, err = retryResult, retryErr
	}
	if err != nil {
		if result.ApprovalCreated {
			return result, fmt.Errorf("auto-merge failed after the approval was posted: %w", err)
		}
		if result.ApprovalAttempted {
			return result, fmt.Errorf("auto-merge failed and GitHub's approval result is unknown: %w", err)
		}
		return result, fmt.Errorf("auto-merge failed: %w", err)
	}
	return result, nil
}

// queuePosition is the " · position 3" suffix for a queued result. GitHub does
// not always report a position, and a missing one must not print as zero.
func queuePosition(result automerge.Result) string {
	if result.Status != automerge.Queued || result.QueuePosition <= 0 {
		return ""
	}
	return fmt.Sprintf(" · position %d", result.QueuePosition)
}

package cli

import (
	"context"
	"fmt"

	"github.com/yungweng/quorum/internal/automerge"
	"github.com/yungweng/quorum/internal/gh"
)

func (a *app) autoMerge(ctx context.Context, client *gh.Client, repo string, number int, sha string) (automerge.Result, error) {
	result, err := automerge.Run(ctx, client, repo, number, sha)
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

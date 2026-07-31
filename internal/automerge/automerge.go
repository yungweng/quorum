// Package automerge approves and merges one reviewed pull request head.
package automerge

import (
	"context"
	"fmt"
	"strings"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/review"
)

const approvalBody = "No blockers or critical findings found."

const (
	Merged = "merged"
)

type Result struct {
	ApprovalAttempted bool
	ApprovalCreated   bool
	Status            string
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
	if merged, err := validateHead(ctx, client, pr, repo, number, reviewedSHA); merged || err != nil {
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
	approved := false
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
		if merged, err := validateHead(ctx, client, current, repo, number, reviewedSHA); merged || err != nil {
			if merged {
				result.Status = Merged
			}
			return result, err
		}
		result.ApprovalAttempted = true
		if err := client.ApproveHead(ctx, repo, number, reviewedSHA, approvalBody); err != nil {
			return result, fmt.Errorf("approving reviewed head: %w", err)
		}
		result.ApprovalCreated = true
	}

	// Re-read the head after approval, then use GitHub's head-bound merge API.
	current, err := client.PRDetails(ctx, repo, number)
	if err != nil {
		return result, err
	}
	if merged, err := validateHead(ctx, client, current, repo, number, reviewedSHA); merged || err != nil {
		if merged {
			result.Status = Merged
		}
		return result, err
	}
	if current.AutoMergeRequest != nil {
		if err := client.DisableAutoMerge(ctx, repo, number); err != nil {
			return result, fmt.Errorf("disabling the existing auto-merge request: %w", err)
		}
		current, err = client.PRDetails(ctx, repo, number)
		if err != nil {
			return result, err
		}
		if merged, err := validateHead(ctx, client, current, repo, number, reviewedSHA); merged || err != nil {
			if merged {
				result.Status = Merged
			}
			return result, err
		}
	}
	if err := client.MergeHead(ctx, repo, number, reviewedSHA); err != nil {
		if current, inspectErr := client.PRDetails(ctx, repo, number); inspectErr == nil {
			if current.State == gh.StateMerged {
				result.Status = Merged
				return result, nil
			}
		}
		return result, fmt.Errorf("merging reviewed head: %w", err)
	}
	result.Status = Merged
	return result, nil
}

func validateHead(ctx context.Context, client *gh.Client, current gh.Details, repo string, number int, reviewedSHA string) (bool, error) {
	if current.State == gh.StateMerged {
		return true, nil
	}
	if current.State != gh.StateOpen {
		return false, fmt.Errorf("pull request %s#%d is %s, not open", repo, number, strings.ToLower(current.State))
	}
	if current.HeadRefOid != reviewedSHA {
		if current.AutoMergeRequest != nil {
			if err := client.DisableAutoMerge(ctx, repo, number); err != nil {
				return false, fmt.Errorf("refusing auto-merge after head drift and disabling the existing request: %w", err)
			}
		}
		return false, fmt.Errorf("refusing auto-merge: reviewed head is %s but GitHub reports %s", reviewedSHA, current.HeadRefOid)
	}
	return false, nil
}

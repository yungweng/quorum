// Package automerge approves a reviewed pull request head and asks GitHub to
// merge it once the repository's own rules pass.
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
	Requested = "requested"
	Merged    = "merged"
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

// Run binds both side effects to reviewedSHA. It is safe to repeat: an
// existing approval for that commit is reused, and an existing auto-merge
// request is accepted as success.
func Run(ctx context.Context, client *gh.Client, repo string, number int, reviewedSHA string) (Result, error) {
	var result Result
	if repo == "" || number <= 0 || reviewedSHA == "" {
		return result, fmt.Errorf("auto-merge needs a repository, pull request number, and reviewed head sha")
	}

	pr, err := client.PRDetails(ctx, repo, number)
	if err != nil {
		return result, err
	}
	if pr.State == gh.StateMerged {
		result.Status = Merged
		return result, nil
	}
	if pr.State != gh.StateOpen {
		return result, fmt.Errorf("pull request %s#%d is %s, not open", repo, number, strings.ToLower(pr.State))
	}
	if pr.HeadRefOid != reviewedSHA {
		return result, fmt.Errorf("refusing auto-merge: reviewed head is %s but GitHub reports %s", reviewedSHA, pr.HeadRefOid)
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
		result.ApprovalAttempted = true
		if err := client.ApproveHead(ctx, repo, number, reviewedSHA, approvalBody); err != nil {
			return result, fmt.Errorf("approving reviewed head: %w", err)
		}
		result.ApprovalCreated = true
	}

	// Re-read head and auto-merge in one response after the approval. This
	// closes the window where a push could otherwise make an auto-merge request
	// for newer, unreviewed code look like ours.
	current, err := client.PRDetails(ctx, repo, number)
	if err != nil {
		return result, err
	}
	if current.State == gh.StateMerged {
		result.Status = Merged
		return result, nil
	}
	if current.State != gh.StateOpen {
		return result, fmt.Errorf("pull request %s#%d is %s, not open", repo, number, strings.ToLower(current.State))
	}
	if current.HeadRefOid != reviewedSHA {
		return result, fmt.Errorf("refusing auto-merge: reviewed head is %s but GitHub reports %s", reviewedSHA, current.HeadRefOid)
	}
	if current.AutoMergeRequest != nil {
		result.Status = Requested
		return result, nil
	}
	if err := client.EnableAutoMerge(ctx, repo, number, reviewedSHA); err != nil {
		if current, inspectErr := client.PRDetails(ctx, repo, number); inspectErr == nil {
			if current.State == gh.StateMerged {
				result.Status = Merged
				return result, nil
			}
			if current.State == gh.StateOpen && current.HeadRefOid == reviewedSHA && current.AutoMergeRequest != nil {
				result.Status = Requested
				return result, nil
			}
		}
		return result, fmt.Errorf("enabling auto-merge: %w", err)
	}
	result.Status = Requested
	if current, err := client.PRDetails(ctx, repo, number); err == nil && current.State == gh.StateMerged {
		result.Status = Merged
	}
	return result, nil
}

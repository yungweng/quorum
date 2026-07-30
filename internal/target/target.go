// Package target resolves the code a manual review or fix loop should use.
package target

import (
	"context"
	"fmt"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
)

// Target is either a pull request or a pushed branch without an open pull
// request. PR carries the common head/base metadata used by both paths.
type Target struct {
	PR         gh.FullPR
	BranchOnly bool
}

// Resolve selects an explicit pull request, the open pull request for the
// current branch, or the current pushed branch when it has no open pull
// request. branch is for an already-resolved internal branch target; manual
// callers leave it empty so checkout safety checks still run.
func Resolve(
	ctx context.Context,
	ghc *gh.Client,
	gitc git.G,
	repoRoot string,
	number int,
	branch string,
	baseBranch string,
) (Target, error) {
	if number > 0 {
		pr, err := ghc.ViewPR(ctx, repoRoot, number)
		return Target{PR: pr}, err
	}

	checkCheckout := branch == ""
	var headSHA string
	var err error
	if checkCheckout {
		branch, err = gitc.CurrentBranch(ctx, repoRoot)
		if err != nil {
			return Target{}, err
		}
		headSHA, err = pushedHead(ctx, gitc, repoRoot, branch)
		if err != nil {
			return Target{}, err
		}
		prs, err := ghc.OpenPRsForBranch(ctx, repoRoot, branch)
		if err != nil {
			return Target{}, err
		}
		prs, err = matchingPRs(branch, headSHA, prs)
		if err != nil {
			return Target{}, err
		}
		switch len(prs) {
		case 0:
			// Continue below with a branch-only target.
		case 1:
			return Target{PR: prs[0]}, nil
		default:
			return Target{}, fmt.Errorf(
				"branch %s has multiple open pull requests; pass a PR number or URL explicitly",
				branch,
			)
		}
	}

	if baseBranch == "" {
		var err error
		baseBranch, err = ghc.DefaultBranch(ctx, repoRoot)
		if err != nil {
			return Target{}, err
		}
	}
	if branch == baseBranch {
		return Target{}, fmt.Errorf("branch %s is the base branch; there is no branch diff to review", branch)
	}

	if headSHA == "" {
		headSHA, err = pushedHead(ctx, gitc, repoRoot, branch)
		if err != nil {
			return Target{}, err
		}
	}
	baseSHA, err := gitc.LsRemote(ctx, repoRoot, "origin", "refs/heads/"+baseBranch)
	if err != nil {
		return Target{}, err
	}

	if checkCheckout {
		dirty, err := gitc.Dirty(ctx, repoRoot)
		if err != nil {
			return Target{}, err
		}
		if dirty {
			return Target{}, fmt.Errorf(
				"your checkout of %s has uncommitted changes; commit and push them first so quorum sees them",
				branch,
			)
		}
		localSHA, err := gitc.RevParse(ctx, repoRoot, "HEAD")
		if err != nil {
			return Target{}, err
		}
		if localSHA != headSHA {
			return Target{}, fmt.Errorf(
				"local branch %s (%s) differs from origin (%s); push your local commits first",
				branch, localSHA, headSHA,
			)
		}
	}

	pr := gh.FullPR{
		Title:       branch,
		State:       "BRANCH",
		HeadRefName: branch,
		HeadRefOid:  headSHA,
		BaseRefName: baseBranch,
		BaseRefOid:  baseSHA,
	}
	return Target{PR: pr, BranchOnly: true}, nil
}

func pushedHead(ctx context.Context, gitc git.G, repoRoot, branch string) (string, error) {
	headSHA, err := gitc.LsRemote(ctx, repoRoot, "origin", "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf(
			"branch %s is not pushed to origin; commit and push it before running quorum: %w",
			branch, err,
		)
	}
	return headSHA, nil
}

func matchingPRs(branch, headSHA string, candidates []gh.FullPR) ([]gh.FullPR, error) {
	prs := make([]gh.FullPR, 0, len(candidates))
	for _, pr := range candidates {
		if pr.IsCrossRepository || pr.HeadRefName != branch {
			continue
		}
		if pr.HeadRefOid != headSHA {
			return nil, fmt.Errorf(
				"PR #%d head %s differs from origin/%s %s; retry after GitHub updates the PR head",
				pr.Number, pr.HeadRefOid, branch, headSHA,
			)
		}
		prs = append(prs, pr)
	}
	return prs, nil
}

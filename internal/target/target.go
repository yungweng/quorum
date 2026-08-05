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
	if checkCheckout {
		var err error
		branch, err = gitc.CurrentBranch(ctx, repoRoot)
		if err != nil {
			return Target{}, err
		}
		pr, found, err := ghc.CurrentBranchPR(ctx, repoRoot)
		if err != nil {
			return Target{}, err
		}
		if found {
			return Target{PR: pr}, nil
		}
	}
	return resolveBranch(ctx, ghc, gitc, repoRoot, branch, baseBranch, checkCheckout)
}

// ResolveLocal resolves the current pushed branch as a branch-only target and
// deliberately never asks whether it has an open pull request: a local run
// must leave the PR alone even when one exists. The checkout safety checks
// still run.
func ResolveLocal(
	ctx context.Context,
	ghc *gh.Client,
	gitc git.G,
	repoRoot string,
	baseBranch string,
) (Target, error) {
	branch, err := gitc.CurrentBranch(ctx, repoRoot)
	if err != nil {
		return Target{}, err
	}
	return resolveBranch(ctx, ghc, gitc, repoRoot, branch, baseBranch, true)
}

// resolveBranch builds the branch-only target both entry points share.
func resolveBranch(
	ctx context.Context,
	ghc *gh.Client,
	gitc git.G,
	repoRoot string,
	branch string,
	baseBranch string,
	checkCheckout bool,
) (Target, error) {
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

	headSHA, err := pushedHead(ctx, gitc, repoRoot, branch)
	if err != nil {
		return Target{}, err
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
			// A checkout that is merely behind origin loses nothing: the run
			// works on origin's head. Only local commits of its own, which a
			// push from elsewhere would overtake, are refused.
			behind, aerr := gitc.IsAncestor(ctx, repoRoot, localSHA, headSHA)
			if aerr != nil {
				return Target{}, aerr
			}
			if !behind {
				return Target{}, fmt.Errorf(
					"local branch %s (%s) differs from origin (%s); push your local commits first",
					branch, localSHA, headSHA,
				)
			}
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

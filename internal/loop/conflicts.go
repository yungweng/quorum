package loop

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/yungweng/quorum/internal/envexec"
)

// ensureBaseCurrent merges the base branch into the head before a review runs
// whenever the head does not contain the latest fetched base. It reports
// whether the merge moved HEAD, so callers can discard a review of the old
// commit instead of declaring that stale result ready.
//
// Quorum starts the merge itself instead of asking GitHub whether the PR is
// mergeable. GitHub computes that field asynchronously, while the local merge
// gives an immediate answer for both PR and branch-only runs. A clean merge
// needs no fix session. When Git leaves unmerged paths, the shared fix session
// resolves the active merge before the normal test and push barriers run.
func (r *run) ensureBaseCurrent() (bool, error) {
	if !r.o.ResolveConflicts {
		return false, nil
	}
	base := r.pr.BaseRefName
	if err := r.waitActivity("fetching "+base+" for the base update", func() error {
		return r.p.Git.Fetch(r.ctx, r.o.RepoRoot, "origin",
			fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", base, base))
	}); err != nil {
		return false, err
	}
	current, err := r.p.Git.IsAncestor(r.ctx, r.worktree, "origin/"+base, "HEAD")
	if err != nil {
		return false, err
	}
	if current {
		return false, nil
	}
	return r.mergeOutdatedBase(base)
}

func (r *run) mergeOutdatedBase(base string) (bool, error) {
	preSHA, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
	if err != nil {
		return false, err
	}
	r.rep.Step(fmt.Sprintf("Update branch from %s", base))
	r.enter(PhaseBaseUpdate)
	var output bytes.Buffer
	mergeErr := r.waitActivity("merging "+base, func() error {
		return r.env.Run(r.ctx, r.o.FixTimeout, envexec.Cmd{
			Name:   r.p.Git.Bin,
			Args:   []string{"merge", "--no-ff", "-m", fmt.Sprintf("Merge %s into %s", base, r.branch), "origin/" + base},
			Stdout: &output, Stderr: &output,
		})
	})
	if mergeErr == nil {
		return r.finishBaseUpdate(base, preSHA, "", "Base update from "+base)
	}

	unmerged, err := r.p.Git.UnmergedFiles(r.ctx, r.worktree)
	if err != nil {
		return false, fmt.Errorf("inspect failed merge of %s into %s: %w", base, r.branch, err)
	}
	if unmerged == "" {
		message := strings.TrimSpace(output.String())
		if message == "" {
			message = mergeErr.Error()
		}
		message, _, _ = strings.Cut(message, "\n")
		return false, fmt.Errorf("merge %s into %s: %s", base, r.branch, message)
	}
	return r.resolveBaseConflicts(base, preSHA)
}

func (r *run) resolveBaseConflicts(base, preSHA string) (bool, error) {
	r.rep.Step(fmt.Sprintf("Merge conflicts with %s", base))
	r.enter(PhaseConflictFix)
	r.conflictFixes++
	tag := fmt.Sprintf("conflict-fix-%d", r.conflictFixes)
	if err := r.codexCall(tag, conflictFixPrompt(r.pr.Number, base, r.branch, r.target.BranchOnly, r.o.Offline)); err != nil {
		return false, err
	}
	if err := r.questionGate(tag); err != nil {
		return false, err
	}
	if err := r.ensureCommitted(tag); err != nil {
		if unmerged, probeErr := r.p.Git.UnmergedFiles(r.ctx, r.worktree); probeErr == nil && unmerged != "" {
			r.rep.Notify("Konflikte", fmt.Sprintf("%s hat weiterhin ungeloeste Merge-Konflikte", r.targetLabel()))
			return false, fmt.Errorf("%w: %v", ErrConflicts, err)
		}
		return false, err
	}
	label := fmt.Sprintf("Merge conflict fix %d", r.conflictFixes)
	afterSHA, err := r.requireBaseUpdate(base, preSHA)
	if err != nil {
		r.rep.Notify("Konflikte", fmt.Sprintf("%s hat weiterhin ungeloeste Merge-Konflikte", r.targetLabel()))
		return false, fmt.Errorf("%w: %v: %s", ErrConflicts, err, r.targetReference())
	}
	return r.publishBaseUpdate(preSHA, afterSHA, tag, label)
}

func (r *run) finishBaseUpdate(base, preSHA, tag, label string) (bool, error) {
	afterSHA, err := r.requireBaseUpdate(base, preSHA)
	if err != nil {
		return false, err
	}
	return r.publishBaseUpdate(preSHA, afterSHA, tag, label)
}

func (r *run) publishBaseUpdate(preSHA, afterSHA, tag, label string) (bool, error) {
	r.recordRound(label, preSHA)
	if r.o.Offline {
		if tag != "" {
			r.queueFixComment(tag, label, preSHA)
		}
		if err := r.ensureTestsGreen(); err != nil {
			return false, err
		}
		if r.o.DivergenceScan {
			r.traceCIFix(preSHA, afterSHA, baseUpdateTag(tag))
		}
		return true, nil
	}
	if err := r.pushBranchWithFixes(); err != nil {
		return false, err
	}
	if r.o.DivergenceScan {
		r.traceCIFix(preSHA, r.headSHA, baseUpdateTag(tag))
	}
	if tag == "" {
		return true, nil
	}
	return true, r.postFixComment(tag, label, "", "", preSHA)
}

func baseUpdateTag(tag string) string {
	if tag == "" {
		return "base-update"
	}
	return tag
}

func (r *run) requireBaseUpdate(base, preSHA string) (string, error) {
	afterSHA, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
	if err != nil {
		return "", err
	}
	if afterSHA == preSHA {
		return "", fmt.Errorf("merge %s into %s did not move HEAD", base, r.branch)
	}
	merged, err := r.p.Git.IsAncestor(r.ctx, r.worktree, "origin/"+base, "HEAD")
	if err != nil {
		return "", err
	}
	if !merged {
		return "", fmt.Errorf("merge did not include origin/%s", base)
	}
	return afterSHA, nil
}

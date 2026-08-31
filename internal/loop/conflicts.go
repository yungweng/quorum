package loop

import (
	"fmt"
)

// ensureMergeable merges the base branch into the head before a review runs,
// whenever the two conflict.
//
// The check is local (git merge-tree against the fetched base) rather than
// GitHub's mergeable field, because the field is computed asynchronously and
// answers UNKNOWN for minutes after a push, and because a branch-only run has
// no PR to ask about. The resolution goes through the shared fix session and
// the normal push barrier; a session that finishes without producing a
// conflict-free merge stops the run instead of letting a later round review a
// branch that cannot land.
func (r *run) ensureMergeable() error {
	if !r.o.ResolveConflicts {
		return nil
	}
	base := r.pr.BaseRefName
	if err := r.waitActivity("fetching "+base+" for the conflict check", func() error {
		return r.p.Git.Fetch(r.ctx, r.o.RepoRoot, "origin",
			fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", base, base))
	}); err != nil {
		return err
	}
	var conflicted bool
	err := r.waitActivity("checking merge conflicts with "+base, func() error {
		var checkErr error
		conflicted, checkErr = r.p.Git.MergeConflicts(r.ctx, r.worktree, "origin/"+base, "HEAD")
		return checkErr
	})
	if err != nil {
		// An old git without merge-tree --write-tree cannot answer. That is a
		// degraded run, not a broken one; CI and the merge still guard the end.
		r.rep.Warn(fmt.Sprintf("cannot check for merge conflicts with %s: %v; continuing without the check", base, err))
		return nil
	}
	if !conflicted {
		return nil
	}

	r.rep.Step(fmt.Sprintf("Merge conflicts with %s", base))
	r.enter(PhaseConflictFix)
	preSHA, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
	if err != nil {
		return err
	}
	r.conflictFixes++
	tag := fmt.Sprintf("conflict-fix-%d", r.conflictFixes)
	if err := r.codexCall(tag, conflictFixPrompt(r.pr.Number, base, r.branch, r.target.BranchOnly, r.o.Offline)); err != nil {
		return err
	}
	if err := r.questionGate(tag); err != nil {
		return err
	}
	if err := r.ensureCommitted(tag); err != nil {
		return err
	}

	afterSHA, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
	if err != nil {
		return err
	}
	if afterSHA == preSHA {
		r.rep.Notify("Konflikte", fmt.Sprintf("%s hat ungeloeste Merge-Konflikte", r.targetLabel()))
		return fmt.Errorf("%w: the resolution session committed nothing: %s", ErrConflicts, r.targetReference())
	}
	// The verdict comes from the same probe as the trigger, against the same
	// fetched base, so a session that merged but left conflict markers or
	// skipped a file cannot pass on its own say-so.
	still, err := r.p.Git.MergeConflicts(r.ctx, r.worktree, "origin/"+base, "HEAD")
	if err == nil && still {
		r.rep.Notify("Konflikte", fmt.Sprintf("%s hat weiterhin Merge-Konflikte", r.targetLabel()))
		return fmt.Errorf("%w: %s still conflicts with %s after the merge: %s",
			ErrConflicts, r.branch, base, r.targetReference())
	}

	label := fmt.Sprintf("Merge conflict fix %d", r.conflictFixes)
	r.recordRound(label, preSHA)
	if r.o.Offline {
		// The merge commit stays local until the run's single final push.
		r.queueFixComment(tag, label, preSHA)
		if err := r.ensureTestsGreen(); err != nil {
			return err
		}
		if r.o.DivergenceScan {
			r.traceCIFix(preSHA, afterSHA, tag)
		}
		return nil
	}
	if err := r.pushBranchWithFixes(); err != nil {
		return err
	}
	if r.o.DivergenceScan {
		r.traceCIFix(preSHA, r.headSHA, tag)
	}
	return r.postFixComment(tag, label, "", "", preSHA)
}

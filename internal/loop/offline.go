package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yungweng/quorum/internal/review"
)

// finalizeOffline publishes a converged offline run: one optional suggestion
// round, one push, one CI run. It reports whether the run settled - the
// pushed branch still carries the head the clean review described. When CI
// repairs moved the head, the caller sends the run through another review
// round instead of converging on commits no review has seen.
func (r *run) finalizeOffline(res *Result, round int, findings review.Findings, comment string) (bool, error) {
	// A prior terminal suggestion changed the head. Reaching finalization with
	// a clean review now proves that changed head is safe for auto-merge.
	if res.SuggestionCommits {
		res.SuggestionCommits = false
	}
	if suggestionRoundDue(r.o, findings) && !r.suggestionDone {
		// The guard is per run, not per convergence: a CI repair after the
		// push can bring the run back here, and the suggestion triage stays
		// strictly terminal.
		r.suggestionDone = true
		pushed, err := r.suggestionRound(round, findings, comment, findings.HeadSHA)
		if pushed {
			res.SuggestionCommits = true
		}
		if err != nil {
			return false, err
		}
	}
	if err := r.ensureTestsGreen(); err != nil {
		return false, err
	}
	current, err := r.p.Git.RevParse(r.ctx, r.worktree, "HEAD")
	if err != nil {
		return false, err
	}
	if current != findings.HeadSHA {
		r.rep.Info("local test fixes moved the head past the clean review; reviewing the repaired head")
		return false, nil
	}

	r.rep.Step("Push")
	if err := r.pushBranch(); err != nil {
		return false, err
	}
	if err := r.flushFixComments(); err != nil {
		return false, err
	}
	if err := r.postFinalReviewComment(res, round, findings); err != nil {
		return false, err
	}

	if !r.target.BranchOnly {
		prePushSHA := r.headSHA
		r.rep.Step("CI")
		if err := r.ensureCIGreen(); err != nil {
			return false, err
		}
		if r.headSHA != prePushSHA {
			r.rep.Info("CI repairs moved the head past the reviewed commit; reviewing the repaired head")
			return false, nil
		}
		if err := r.requirePublishedHead(r.headSHA); err != nil {
			return false, fmt.Errorf("checking the pull request head after CI: %w", err)
		}
	}
	return true, nil
}

// queueFixComment captures one offline round's fix log for the consolidated
// comment flushed after the final push. Its online counterpart is
// postFixComment; the parts held back here are exactly what that would have
// posted, minus the per-round review backlinks no posted review exists for.
func (r *run) queueFixComment(tag, label, preSHA string) {
	if r.target.BranchOnly || !r.o.Post {
		return
	}
	commits := r.p.Git.LogOneline(r.ctx, r.worktree, preSHA+"..HEAD")
	if commits == "" {
		return
	}
	var original string
	if data, err := os.ReadFile(filepath.Join(r.msgDir, tag+".md")); err == nil {
		original = string(data)
	}
	r.pendingFixComments = append(r.pendingFixComments,
		fixCommentBody(label, "", "", r.lastMsg, original, commits))
}

// flushFixComments posts the queued offline rounds as one comment. The queue
// empties before posting so a later flush cannot duplicate a comment after a
// partial GitHub failure; the caller receives that failure and stops the run.
func (r *run) flushFixComments() error {
	parts := r.pendingFixComments
	r.pendingFixComments = nil
	if len(parts) == 0 || r.target.BranchOnly || !r.o.Post {
		return nil
	}
	if err := r.requirePublishedHead(r.headSHA); err != nil {
		return fmt.Errorf("checking the pull request head before posting offline fix logs: %w", err)
	}
	if _, posted := r.postPRComment("fix-log comment", strings.Join(parts, "\n\n"), ""); !posted {
		return fmt.Errorf("could not post offline fix logs to PR #%d", r.pr.Number)
	}
	return nil
}

// postFinalReviewComment publishes the clean review's comment once its head is
// public. Offline rounds review unpushed commits, so review.Runner could not
// post at review time the way an online round does; the push barrier that just
// completed is what guarantees GitHub now has the reviewed commit.
func (r *run) postFinalReviewComment(res *Result, round int, findings review.Findings) error {
	if r.target.BranchOnly || !r.o.Post || findings.CommentFile == "" {
		return nil
	}
	if findings.HeadSHA != r.headSHA {
		r.rep.Info("suggestion changes moved the head past the clean review; skipping its stale comment")
		return nil
	}
	if err := r.requirePublishedHead(findings.HeadSHA); err != nil {
		return fmt.Errorf("checking the pull request head before posting review round %d's comment: %w", round, err)
	}
	url, err := r.p.GH.Comment(r.ctx, r.o.RepoRoot, r.pr.Number, findings.CommentFile)
	if err != nil {
		return fmt.Errorf("posting review round %d's comment to PR #%d: %w", round, r.pr.Number, err)
	}
	res.LastFindings.Posted = true
	if url != "" {
		res.LastFindings.CommentURL = &url
	}
	return nil
}

func (r *run) requirePublishedHead(expected string) error {
	current, err := r.p.GH.HeadSHA(r.ctx, r.o.RepoRoot, r.pr.Number)
	if err != nil {
		return err
	}
	if current != expected {
		return fmt.Errorf("the pushed head is %s but GitHub reports %s; refusing to post", expected, current)
	}
	return nil
}

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

	r.rep.Step("Push")
	if err := r.pushBranch(); err != nil {
		return false, err
	}
	r.flushFixComments()
	r.postFinalReviewComment(res, round, findings)

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
// empties even when posting fails or is skipped: the rounds it held are
// published (or deliberately unpublished) either way, and a later flush must
// not repost them.
func (r *run) flushFixComments() {
	parts := r.pendingFixComments
	r.pendingFixComments = nil
	if len(parts) == 0 || r.target.BranchOnly || !r.o.Post {
		return
	}
	r.postPRComment("fix-log comment", strings.Join(parts, "\n\n"), "")
}

// postFinalReviewComment publishes the clean review's comment once its head is
// public. Offline rounds review unpushed commits, so review.Runner could not
// post at review time the way an online round does; the push barrier that just
// completed is what guarantees GitHub now has the reviewed commit.
func (r *run) postFinalReviewComment(res *Result, round int, findings review.Findings) {
	if r.target.BranchOnly || !r.o.Post || findings.CommentFile == "" {
		return
	}
	url, err := r.p.GH.Comment(r.ctx, r.o.RepoRoot, r.pr.Number, findings.CommentFile)
	if err != nil {
		r.rep.Warn(fmt.Sprintf("could not post review round %d's comment to PR #%d: %v", round, r.pr.Number, err))
		return
	}
	res.LastFindings.Posted = true
	if url != "" {
		res.LastFindings.CommentURL = &url
	}
}

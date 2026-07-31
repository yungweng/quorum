package history

import (
	"github.com/yungweng/quorum/internal/state"
)

// FromState turns a state file change into a history entry and reports whether
// it is one worth recording.
//
// This is the only place history knows about the state file. It lives here
// rather than in the two agent call sites so that both go through the same
// rule, because the interesting part is not the conversion but which changes
// count, and getting that wrong in one of the two is invisible until the log
// is missing runs.
//
// A finished or failed review is recorded once per run, and two reviews of the
// same pull request stay two entries, which is exactly what the state file
// cannot express. What separates the two is At: Mark stamps it when a run ends,
// while bookkeeping writes such as caching a timeline lookup deliberately leave
// it alone. So a write that finds the record already finished and does not move
// At is not a run ending, it is the same run being touched again. Recording
// those is how one review ends up on the dashboard four times: every poll after
// it, the pull request's updatedAt has moved (its own review comment moved it),
// the timeline is read again, and the cached answer is written back over an
// untouched "ok".
//
// Skipped and gave up are decisions rather than runs, and the poll re-applies
// them on every pass, so they are recorded only when the decision itself is
// new. Everything else is a stage of a run in progress and belongs on the
// dashboard, not in the log.
func FromState(key string, before, after state.Record, source string) (Run, bool) {
	switch after.Status {
	case state.OK, state.Failed:
		if before.Status == after.Status && before.At == after.At {
			return Run{}, false
		}
	case state.Skipped, state.GaveUp:
		if before.Status == after.Status && before.Reason == after.Reason {
			return Run{}, false
		}
	default:
		return Run{}, false
	}

	run := Run{
		Key:        key,
		Title:      after.Title,
		Kind:       KindReview,
		Source:     source,
		Outcome:    outcome(after.Status),
		Reason:     after.Reason,
		StartedAt:  state.Entry{Record: after}.Started(),
		EndedAt:    state.Entry{Record: after}.Time(),
		CommentURL: after.CommentURL,
		RunDir:     after.RunDir,
	}
	if after.Status == state.OK {
		run.Reviewed = true
		run.Blockers = int(after.Blockers)
		run.Critical = int(after.Critical)
		run.Suggestions = int(after.Suggestions)
		run.Questions = int(after.Questions)
	}
	return run, true
}

func outcome(status string) string {
	switch status {
	case state.OK:
		return OK
	case state.Failed:
		return Failed
	case state.Skipped:
		return Skipped
	case state.GaveUp:
		return GaveUp
	}
	return status
}

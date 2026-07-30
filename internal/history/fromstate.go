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
// A finished or failed review is always recorded: those are runs, and two
// reviews of the same pull request have to stay two entries, which is exactly
// what the state file cannot express. Skipped and gave up are decisions rather
// than runs, and the poll re-applies them on every pass, so they are recorded
// only when the decision itself is new. Everything else is a stage of a run in
// progress and belongs on the dashboard, not in the log.
func FromState(key string, before, after state.Record, source string) (Run, bool) {
	switch after.Status {
	case state.OK, state.Failed:
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

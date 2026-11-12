package history

import (
	"github.com/yungweng/quorum/internal/state"
)

// Record applies fn to the state record for key and adds the change to the
// history log when it is one the log wants. Every agent write goes through
// here, which is what keeps the two stores from drifting apart and makes it
// impossible to add an outcome later that quietly never gets logged.
//
// The two errors are separate because they cost differently: a lost state
// write loses the outcome of a review that has already been paid for, while a
// history log that could not be extended is worth a line, not a failure.
func Record(statePath, historyPath, key, source string, fn func(*state.Record)) (stateErr, historyErr error) {
	var before, after state.Record
	capture := func(rec *state.Record) {
		before = *rec
		fn(rec)
		after = *rec
	}
	if err := state.Mutate(statePath, key, capture); err != nil {
		return err, nil
	}
	if run, ok := fromState(key, before, after, source); ok {
		return nil, Append(historyPath, run)
	}
	return nil, nil
}

// fromState turns a state file change into a history entry and reports whether
// it is one worth recording. The interesting part is not the conversion but
// which changes count, and getting that wrong is invisible until the log is
// missing runs.
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
func fromState(key string, before, after state.Record, source string) (Run, bool) {
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
		Author:     after.Author,
		Kind:       KindReview,
		Source:     source,
		Outcome:    outcome(after.Status),
		Reason:     after.Reason,
		StartedAt:  state.Entry{Record: after}.Started(),
		EndedAt:    state.Entry{Record: after}.Time(),
		CommentURL: after.CommentURL,
		RunDir:     after.RunDir,
	}
	// A fix loop the agent ran is a fix loop in the log, with its rounds, the
	// same as one started from a terminal. Skips and give-ups are decisions
	// about a pull request, whatever ran on it before.
	if after.Kind == KindBabysit && (after.Status == state.OK || after.Status == state.Failed) {
		run.Kind = KindBabysit
		run.Rounds = int(after.Rounds)
		if after.Status == state.OK && after.Converged {
			run.Outcome = Converged
		}
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

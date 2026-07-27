package loop

import (
	"strings"
	"testing"
)

// The markers are a contract with the fix session. Matching them loosely would
// be worse than not matching at all: a finding that merely mentions "DISPUTED
// FINDINGS:" in its text would end a round without any code changing.
func TestMarkersMatchOnlyAtTheStartOfALine(t *testing.T) {
	msg := `I fixed the migration ordering and the retry bound.

PR COMMENT:
Fixed the migration ordering.`

	if !hasMarker(msg, MarkerComment) {
		t.Error("a marker at the start of a line was not found")
	}
	if hasMarker(msg, MarkerDisputed) {
		t.Error("a marker that is not present was found")
	}

	inline := "The reviewer wrote DISPUTED FINDINGS: in their comment, which I disagree with."
	if hasMarker(inline, MarkerDisputed) {
		t.Error("a marker mentioned mid-sentence was treated as the real marker")
	}
}

// section returns the marker line and everything after it, which is what gets
// shown to the user and fed back into the session.
func TestSectionReturnsTheMarkerAndEverythingAfter(t *testing.T) {
	msg := `Summary of the round.

DISPUTED FINDINGS:
1. The parser already handles empty input.
2. The retry loop is bounded by the context.`

	got := section(msg, MarkerDisputed)
	if !strings.HasPrefix(got, MarkerDisputed) {
		t.Errorf("section did not start at the marker: %q", got)
	}
	if !strings.Contains(got, "2. The retry loop") {
		t.Error("section dropped part of the block")
	}
	if strings.Contains(got, "Summary of the round") {
		t.Error("section included text from before the marker")
	}
	if section(msg, MarkerQuestions) != "" {
		t.Error("an absent marker did not return the empty string")
	}
}

// Autonomous mode must tell the session to decide for itself; interactive mode
// must leave it the option to ask. Getting this backwards means either a run
// that stalls on questions nobody will answer, or a session inventing product
// decisions while a human sits waiting.
func TestStandingRulesSwitchOnAutonomy(t *testing.T) {
	auto := standingRules("feature/calendar", true)
	if !strings.Contains(auto, "No human is available during this run") {
		t.Error("autonomous rules do not tell the session to decide itself")
	}
	if strings.Contains(auto, "stop and end your final message with a line that is exactly:\nOPEN QUESTIONS:") {
		t.Error("autonomous rules still invite the session to ask questions")
	}

	interactive := standingRules("feature/calendar", false)
	if !strings.Contains(interactive, MarkerQuestions) {
		t.Error("interactive rules do not describe the questions marker")
	}
}

// The push command is spelled out because a bare `git push` fails on the
// detached checkout the pipeline works in.
func TestStandingRulesCarryTheExactPushCommand(t *testing.T) {
	rules := standingRules("feature/calendar", true)
	want := "git push origin HEAD:refs/heads/feature/calendar"
	if !strings.Contains(rules, want) {
		t.Errorf("the standing rules do not contain %q", want)
	}
}

// The pipeline posts the fix log itself so it appears as an ordinary comment
// from the user. A session that posts its own would produce duplicates.
func TestStandingRulesForbidTheSessionPostingComments(t *testing.T) {
	rules := standingRules("main", true)
	if !strings.Contains(rules, "Never create or edit PRs or comments") {
		t.Error("the standing rules do not forbid the session from commenting")
	}
	if !strings.Contains(rules, "Never mention AI, agents, Codex, or automation") {
		t.Error("the standing rules do not forbid mentioning automation")
	}
}

// A fix round must be told not to wait for CI: the pipeline watches the checks,
// and an earlier version had the session waiting too, which was idle time
// billed to the fix session.
func TestFixPromptsTellTheSessionNotToWaitForCI(t *testing.T) {
	for name, prompt := range map[string]string{
		"ci fix":    ciFixPrompt(12, "[]"),
		"fix round": fixRoundPrompt(12, "## Summary\n\nfine"),
	} {
		if !strings.Contains(prompt, "Do not wait for CI") {
			t.Errorf("%s prompt does not tell the session to leave CI to the pipeline", name)
		}
	}
}

func TestPRContextIncludesExtraUserContext(t *testing.T) {
	got := prContext(7, "Add the kit picker", "feature/kit", "main",
		"https://github.com/acme/api/pull/7", "Body text.", "Focus on the calendar")
	for _, want := range []string{"PR #7", "feature/kit", "Body text.", "Focus on the calendar"} {
		if !strings.Contains(got, want) {
			t.Errorf("context is missing %q", want)
		}
	}

	without := prContext(7, "t", "b", "main", "u", "Body text.", "")
	if strings.Contains(without, "Additional context") {
		t.Error("an empty extra context still produced a section")
	}
}

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
	auto := standingRules("feature/crumb-tray", true, false, "")
	if !strings.Contains(auto, "No human is available during this run") {
		t.Error("autonomous rules do not tell the session to decide itself")
	}
	if strings.Contains(auto, "stop and end your final message with a line that is exactly:\nOPEN QUESTIONS:") {
		t.Error("autonomous rules still invite the session to ask questions")
	}

	interactive := standingRules("feature/crumb-tray", false, false, "")
	if !strings.Contains(interactive, MarkerQuestions) {
		t.Error("interactive rules do not describe the questions marker")
	}
}

// The push command is spelled out because a bare `git push` fails on the
// detached checkout the pipeline works in.
func TestStandingRulesCarryTheExactPushCommand(t *testing.T) {
	rules := standingRules("feature/crumb-tray", true, false, "")
	want := "git push origin HEAD:refs/heads/feature/crumb-tray"
	if !strings.Contains(rules, want) {
		t.Errorf("the standing rules do not contain %q", want)
	}
}

// The pipeline posts the fix log itself so it appears as an ordinary comment
// from the user. A session that posts its own would produce duplicates.
func TestStandingRulesForbidTheSessionPostingComments(t *testing.T) {
	rules := standingRules("main", true, false, "")
	if !strings.Contains(rules, "Never create or edit PRs or comments") {
		t.Error("the standing rules do not forbid the session from commenting")
	}
	if !strings.Contains(rules, "Never mention AI, agents, Codex, or automation") {
		t.Error("the standing rules do not forbid mentioning automation")
	}
}

func TestFixSessionPromptsRequireACleanWorktree(t *testing.T) {
	rules := standingRules("main", true, false, "")
	for _, want := range []string{"$TMPDIR or /tmp", "leave git status --porcelain empty", "remove only temporary artifacts you created"} {
		if !strings.Contains(rules, want) {
			t.Errorf("the standing rules are missing %q", want)
		}
	}
	for _, want := range []string{"$TMPDIR or /tmp", "empty git status --porcelain"} {
		if !strings.Contains(commitPrompt, want) {
			t.Errorf("the commit prompt is missing %q", want)
		}
	}
}

// A fix round must be told not to wait for CI: the pipeline watches the checks,
// and an earlier version had the session waiting too, which was idle time
// billed to the fix session.
func TestFixPromptsTellTheSessionNotToWaitForCI(t *testing.T) {
	for name, prompt := range map[string]string{
		"ci fix":           ciFixPrompt(12, "[]"),
		"fix round":        fixRoundPrompt(12, "", false, "## Summary\n\nfine"),
		"suggestion round": suggestionRoundPrompt(12, "", false, "## Summary\n\nfine"),
	} {
		if !strings.Contains(prompt, "Do not wait for CI") {
			t.Errorf("%s prompt does not tell the session to leave CI to the pipeline", name)
		}
	}
}

func TestPRFixPromptsRequireACommentBlock(t *testing.T) {
	for name, prompt := range map[string]string{
		"ci fix":           ciFixPrompt(12, "[]"),
		"review fix":       fixRoundPrompt(12, "", false, "## Summary\n\nfine"),
		"suggestion round": suggestionRoundPrompt(12, "", false, "## Summary\n\nfine"),
	} {
		if !strings.Contains(prompt, "a line that is exactly:\n"+MarkerComment) {
			t.Errorf("%s prompt does not require the PR comment marker", name)
		}
		for _, want := range []string{"what", "which checks you ran", "language of the PR description", "never mention AI"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s prompt is missing %q from the comment contract", name, want)
			}
		}
	}
}

func TestBranchFixPromptDoesNotInventAPROrCIWatcher(t *testing.T) {
	got := fixRoundPrompt(0, "feature/crumb-tray", true, "## Summary\n\nfine")
	for _, unwanted := range []string{"PR #0", "pipeline watches the checks", MarkerComment} {
		if strings.Contains(got, unwanted) {
			t.Errorf("branch fix prompt contains %q", unwanted)
		}
	}
	for _, want := range []string{"branch feature/crumb-tray", "Run the affected checks"} {
		if !strings.Contains(got, want) {
			t.Errorf("branch fix prompt is missing %q", want)
		}
	}
}

func TestBranchContextDoesNotInventAPR(t *testing.T) {
	got := branchContext("feature/crumb-tray", "main", "Focus on cleanup")
	for _, want := range []string{"feature/crumb-tray", "main", "No open pull request", "Focus on cleanup"} {
		if !strings.Contains(got, want) {
			t.Errorf("branch context is missing %q", want)
		}
	}
	if strings.Contains(got, "PR #0") {
		t.Error("branch context invented PR #0")
	}
}

func TestPRContextIncludesExtraUserContext(t *testing.T) {
	got := prContext(7, "Add the browning dial", "feature/browning", "main",
		"https://github.com/acme/api/pull/7", "Body text.", "Focus on the crumb tray")
	for _, want := range []string{"PR #7", "feature/browning", "Body text.", "Focus on the crumb tray"} {
		if !strings.Contains(got, want) {
			t.Errorf("context is missing %q", want)
		}
	}

	without := prContext(7, "t", "b", "main", "u", "Body text.", "")
	if strings.Contains(without, "Additional context") {
		t.Error("an empty extra context still produced a section")
	}
}

func TestFinalDescriptionPromptDescribesStateWithoutHistory(t *testing.T) {
	got := finalDescriptionPrompt(42, "Bound retries", "main")
	for _, want := range []string{
		"finished local diff against origin/main",
		"must stand alone",
		"Do not write a changelog or development narrative",
		"materially departs from that intent",
		"one short blockquote",
		"Do not exaggerate",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("final description prompt is missing %q", want)
		}
	}
	for _, unwanted := range []string{"always add a warning", "list every fix", "summarize the review rounds"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("final description prompt contains %q", unwanted)
		}
	}
}

// Repository rules join the standing rules so a fix round cannot resolve a
// finding in a way that itself violates the rule that produced it.
func TestStandingRulesCarryRepositoryRules(t *testing.T) {
	repoRules := "- No new UI components; reuse existing ones first (Blocker)."
	rules := standingRules("main", true, false, repoRules)
	if !strings.Contains(rules, repoRules) {
		t.Error("the standing rules are missing the repository rules")
	}
	if !strings.Contains(rules, "never resolve a finding in a way that itself violates one of them") {
		t.Error("the repository rules block lost its framing")
	}
	if strings.Contains(standingRules("main", true, false, ""), "its own rules below") {
		t.Error("a rules block appeared without repository rules")
	}
}

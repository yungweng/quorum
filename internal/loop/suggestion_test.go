package loop

import (
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/review"
)

// The suggestion round must stay strictly terminal: it never runs while
// Blockers or Critical findings remain, never runs when disabled, and never
// runs without Suggestions. Anything else would turn Suggestions into a count
// that keeps the loop alive.
func TestSuggestionRoundDue(t *testing.T) {
	on := Options{FixSuggestions: true}
	for _, tc := range []struct {
		name     string
		o        Options
		findings review.Findings
		want     bool
	}{
		{"clean review with suggestions", on, review.Findings{Suggestions: 2}, true},
		{"disabled", Options{}, review.Findings{Suggestions: 2}, false},
		{"no suggestions", on, review.Findings{Questions: 1}, false},
		{"blockers remain", on, review.Findings{Blockers: 1, Suggestions: 2}, false},
		{"critical remains", on, review.Findings{Critical: 1, Suggestions: 2}, false},
	} {
		if got := suggestionRoundDue(tc.o, tc.findings); got != tc.want {
			t.Errorf("%s: suggestionRoundDue = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// No review follows the suggestion round, so the prompt has to ask for triage
// and explicitly allow ending without any change; a session forced to produce
// commits would invent them.
func TestSuggestionRoundPromptDemandsTriageAndAllowsNoChange(t *testing.T) {
	for name, prompt := range map[string]string{
		"pr":     suggestionRoundPrompt(12, "", false, false, "## Summary\n\nfine"),
		"branch": suggestionRoundPrompt(0, "feature/crumb-tray", true, false, "## Summary\n\nfine"),
	} {
		for _, want := range []string{
			"first decide whether it is worth implementing",
			"Changing nothing is a perfectly fine outcome",
			"no further review will check your work",
			"## Summary",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("%s suggestion prompt is missing %q", name, want)
			}
		}
	}
}

func TestSuggestionRoundBranchPromptDoesNotInventAPROrCIWatcher(t *testing.T) {
	got := suggestionRoundPrompt(0, "feature/crumb-tray", true, false, "## Summary\n\nfine")
	for _, unwanted := range []string{"PR #0", "pipeline watches the checks", MarkerComment} {
		if strings.Contains(got, unwanted) {
			t.Errorf("branch suggestion prompt contains %q", unwanted)
		}
	}
	if !strings.Contains(got, "branch feature/crumb-tray") {
		t.Error("branch suggestion prompt does not name the branch")
	}
}

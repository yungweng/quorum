package review

import (
	"strings"
	"testing"
)

func TestAggregatorPromptForBranchProducesALocalReport(t *testing.T) {
	got := aggregatorPrompt(promptMeta{
		Title:      "feature/crumb-tray",
		Branch:     "feature/crumb-tray",
		BranchOnly: true,
		BaseRef:    "origin/main",
		BaseSHA:    "base-sha",
		HeadSHA:    "head-sha",
	})
	for _, want := range []string{"local branch review report", "Branch: feature/crumb-tray", "## Blockers"} {
		if !strings.Contains(got, want) {
			t.Errorf("branch aggregator prompt is missing %q", want)
		}
	}
	for _, unwanted := range []string{"GitHub PR comment", "Author: @", "Address @ directly"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("branch aggregator prompt contains %q", unwanted)
		}
	}
}

func TestVerifierPromptIsNeutralAndAllowsEvidenceBasedEditing(t *testing.T) {
	got := verifierPrompt(promptMeta{
		URL: "https://example.invalid/acme/api/pull/42", Title: "Bound retries",
		Author: "example-user", BaseRef: "origin/main", BaseSHA: "base-sha", HeadSHA: "head-sha",
	})
	for _, want := range []string{
		"Some may be true positives, some may be false positives",
		"Do not aim for any predetermined outcome or proportion",
		"You may run focused commands or tests",
		"Do not edit source, tests, configuration",
		"Preserve it unchanged",
		"Rewrite it",
		"Remove the entire finding",
		"Add a new finding only",
		"Do not favor keeping, changing, adding, or removing findings",
		`The very first line of your answer must be exactly "## Summary"`,
		"Do not write any preamble, plan, status update, or tool narration",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("verifier prompt is missing %q", want)
		}
	}
	for _, biased := range []string{"adversarial false-positive verifier", "optimizes against false positives", "When evidence is ambiguous, remove it"} {
		if strings.Contains(got, biased) {
			t.Errorf("verifier prompt contains biased instruction %q", biased)
		}
	}
}

// Repository rules must survive the aggregator's "drop polish, taste, naming"
// rule and the verifier's scope rule; both prompts state that rule findings
// are real at the severity the rule names. A repo without rules gets the
// prompts it always got.
func TestPromptsCarryRepositoryRules(t *testing.T) {
	rules := "- No new UI components; reuse existing ones first (Blocker)."
	meta := promptMeta{
		URL: "https://example.invalid/acme/api/pull/42", Title: "Bound retries",
		Author: "example-user", BaseRef: "origin/main", BaseSHA: "base-sha", HeadSHA: "head-sha",
		Rules: rules,
	}
	for name, prompt := range map[string]string{
		"aggregator": aggregatorPrompt(meta),
		"verifier":   verifierPrompt(meta),
	} {
		if !strings.Contains(prompt, rules) {
			t.Errorf("%s prompt is missing the rules text", name)
		}
		if !strings.Contains(prompt, "severity the rule states") {
			t.Errorf("%s prompt is missing the severity framing", name)
		}
	}

	meta.Rules = ""
	for name, prompt := range map[string]string{
		"aggregator": aggregatorPrompt(meta),
		"verifier":   verifierPrompt(meta),
	} {
		if strings.Contains(prompt, "Repository-specific review rules") {
			t.Errorf("%s prompt grew a rules block without rules", name)
		}
	}
}

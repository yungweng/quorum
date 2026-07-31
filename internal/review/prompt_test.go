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

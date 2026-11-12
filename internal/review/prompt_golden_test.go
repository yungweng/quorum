package review

import (
	"os"
	"path/filepath"
	"testing"
)

// goldenPrompts are the aggregator and verifier prompts for a pull request
// and for a branch. The engines parse nothing back from a prompt, but the
// five-section contract is stated in it, so its text is pinned as a whole.
func goldenPrompts() map[string]string {
	pr := promptMeta{
		URL: "https://example.invalid/acme/api/pull/42", Title: "Bound retries",
		Author: "example-user", BaseRef: "origin/main", BaseSHA: "base-sha", HeadSHA: "head-sha",
		Rules: "- No new UI components; reuse existing ones first (Blocker).",
	}
	branch := promptMeta{
		Title: "feature/crumb-tray", Branch: "feature/crumb-tray", BranchOnly: true,
		BaseRef: "origin/main", BaseSHA: "base-sha", HeadSHA: "head-sha",
	}
	return map[string]string{
		"aggregator-pr.txt":     aggregatorPrompt(pr),
		"aggregator-branch.txt": aggregatorPrompt(branch),
		"verifier-pr.txt":       verifierPrompt(pr),
		"verifier-branch.txt":   verifierPrompt(branch),
	}
}

func TestPromptsMatchTheirGoldenText(t *testing.T) {
	for name, got := range goldenPrompts() {
		want, err := os.ReadFile(filepath.Join("testdata", "prompts", name))
		if err != nil {
			t.Fatal(err)
		}
		if got != string(want) {
			t.Errorf("%s differs from testdata/prompts/%s", name, name)
		}
	}
}

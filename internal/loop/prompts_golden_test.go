package loop

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The prompts are what the fix session acts on, so their exact text is part of
// the pipeline's behaviour. Every PR/branch and online/offline combination is
// pinned byte for byte against testdata/prompts.
func TestPromptsMatchTheirGoldenText(t *testing.T) {
	const comment = "## Summary\n\nfine"
	for _, branchOnly := range []bool{false, true} {
		for _, offline := range []bool{false, true} {
			pt := promptTarget{number: 42, branch: "feature/crumb-tray", branchOnly: branchOnly, offline: offline}
			name := fmt.Sprintf("branchonly-%t-offline-%t", branchOnly, offline)
			got := map[string]string{
				"rules-autonomous":  standingRules(pt, true, ""),
				"rules-interactive": standingRules(pt, false, ""),
				"rules-repo":        standingRules(pt, true, "Never log tokens."),
				"fix-round":         fixRoundPrompt(pt, comment),
				"suggestion-round":  suggestionRoundPrompt(pt, comment),
				"conflict-fix":      conflictFixPrompt(pt, "main"),
			}
			for kind, text := range got {
				path := filepath.Join("testdata", "prompts", name+"-"+kind+".txt")
				want, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				if text != string(want) {
					t.Errorf("%s differs from %s:\n%s", kind, path, text)
				}
			}
		}
	}
}

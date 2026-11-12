package headless

import (
	"strings"
	"testing"
)

func TestClassifyRecognisesQuotaRefusals(t *testing.T) {
	for _, refusal := range []string{
		"Claude usage limit reached, resets at 3am",
		"Grok usage limit reached, try again later",
	} {
		if classify(refusal) == nil {
			t.Errorf("usage limit line %q was not classified", refusal)
		}
	}
	// A per-minute throttle is transient, not account exhaustion: with every
	// reviewer starting at once it happens routinely and must not pause the
	// agent.
	if classify("API rate limit exceeded") != nil {
		t.Error("a transient rate-limit throttle was classified as a quota refusal")
	}
	if classify("something else entirely broke") != nil {
		t.Error("an unrelated failure was classified as a quota refusal")
	}
}

// Repository rules ride into the reviewer prompt as additional criteria; a
// repo without rules gets exactly the prompt it always got.
func TestReviewPromptCarriesRepositoryRules(t *testing.T) {
	rules := "- No new UI components; reuse existing ones first (Blocker)."
	with := reviewPrompt("origin/main", "diff", rules)
	if !strings.Contains(with, rules) {
		t.Errorf("rules missing from prompt: %s", with)
	}
	if !strings.Contains(with, "real finding you must report") {
		t.Error("rules block lost its severity framing")
	}
	without := reviewPrompt("origin/main", "diff", "")
	if strings.Contains(without, "its own review rules") {
		t.Errorf("rules block appeared without rules: %s", without)
	}
}

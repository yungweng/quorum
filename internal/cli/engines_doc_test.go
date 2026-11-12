package cli

import (
	"regexp"
	"testing"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/engine/spec"
)

// The usage texts name the engines and their effort levels by hand. An engine
// or effort added to spec alone still builds and passes every other test, so
// this checks both texts mention each one.
func TestUsageMentionsEveryEngineAndEffort(t *testing.T) {
	a := &app{cfg: config.Default()}
	for name, help := range map[string]string{
		"review":  a.reviewUsageText(),
		"babysit": a.babysitUsageText(),
	} {
		for _, eng := range spec.All {
			if !mentions(help, eng.Name) {
				t.Errorf("%s usage does not mention the engine %q", name, eng.Name)
			}
			for _, effort := range eng.Efforts {
				if !mentions(help, effort) {
					t.Errorf("%s usage does not mention %s's effort %q", name, eng.Name, effort)
				}
			}
		}
	}
}

func mentions(text, word string) bool {
	return regexp.MustCompile(`\b` + regexp.QuoteMeta(word) + `\b`).MatchString(text)
}

package config

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/engine/spec"
)

var assignedKey = regexp.MustCompile(`(?m)^([A-Z][A-Z_]*)=`)

// The reference shows the config file by hand. A key added to render alone
// still round-trips and passes every other test, so the documented keys are
// compared with the ones render writes.
func TestReferenceDocumentsEveryRenderedKey(t *testing.T) {
	doc, err := os.ReadFile("../../docs/reference.md")
	if err != nil {
		t.Fatal(err)
	}
	section := string(doc)
	section = section[strings.Index(section, "\n## Configuration\n"):]
	section = section[:strings.Index(section, "```\n")]

	if rendered, documented := keysIn(Default().render()), keysIn(section); !slices.Equal(rendered, documented) {
		t.Errorf("render writes %v\ndocs/reference.md shows %v", rendered, documented)
	}
}

func keysIn(text string) []string {
	var keys []string
	for _, m := range assignedKey.FindAllStringSubmatch(text, -1) {
		keys = append(keys, m[1])
	}
	sort.Strings(keys)
	return keys
}

// The rendered config's comments name the engines by hand, so an engine added
// to spec alone would leave them stale.
func TestRenderMentionsEveryEngine(t *testing.T) {
	rendered := Default().render()
	for _, eng := range spec.All {
		if !regexp.MustCompile(`\b` + regexp.QuoteMeta(eng.Name) + `\b`).MatchString(rendered) {
			t.Errorf("the rendered config does not mention the engine %q", eng.Name)
		}
	}
}

package cli

import (
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/config"
)

var (
	helpFlag  = regexp.MustCompile(`(?m)^  (-[a-zA-Z], )?--([a-z][a-z-]*)`)
	helpShort = regexp.MustCompile(`(?m)^  -([a-zA-Z]), --`)
	docFlag   = regexp.MustCompile("(?m)^\\| (?:`-([a-zA-Z])`, )?`--([a-z][a-z-]*)")
)

// A flag exists in three places: the set the parser accepts, the help text and
// the reference. One added to a single place still builds and still passes
// every other test, so this compares all three.
func TestFlagsMatchHelpAndReference(t *testing.T) {
	reference, err := os.ReadFile("../../docs/reference.md")
	if err != nil {
		t.Fatal(err)
	}
	a := &app{cfg: config.Default()}
	for _, c := range []struct {
		name    string
		flags   flagSet
		retired []string
		help    string
	}{
		{"review", reviewFlags, reviewRetiredFlags, a.reviewUsageText()},
		{"babysit", babysitFlags, nil, a.babysitUsageText()},
	} {
		t.Run(c.name, func(t *testing.T) {
			var accepted []string
			for name := range c.flags {
				if !slices.Contains(c.retired, name) {
					accepted = append(accepted, name)
				}
			}
			sort.Strings(accepted)

			options := c.help[strings.Index(c.help, "Options:"):]
			if got := flagNames(helpFlag, helpShort, options); !slices.Equal(got, accepted) {
				t.Errorf("help lists %v, the parser accepts %v", got, accepted)
			}

			section := referenceSection(t, string(reference), "## quorum "+c.name)
			if got := flagNames(docFlag, nil, section); !slices.Equal(got, accepted) {
				t.Errorf("docs/reference.md lists %v, the parser accepts %v", got, accepted)
			}
		})
	}
}

// flagNames returns the sorted long and short option names a text documents.
func flagNames(long, short *regexp.Regexp, text string) []string {
	var names []string
	for _, m := range long.FindAllStringSubmatch(text, -1) {
		names = append(names, m[len(m)-1])
		if short == nil && m[1] != "" {
			names = append(names, m[1])
		}
	}
	if short != nil {
		for _, m := range short.FindAllStringSubmatch(text, -1) {
			names = append(names, m[1])
		}
	}
	sort.Strings(names)
	return names
}

func referenceSection(t *testing.T, doc, heading string) string {
	t.Helper()
	start := strings.Index(doc, heading+"\n")
	if start < 0 {
		t.Fatalf("docs/reference.md has no %q section", heading)
	}
	rest := doc[start+len(heading):]
	if end := strings.Index(rest, "\n## "); end >= 0 {
		rest = rest[:end]
	}
	return rest
}

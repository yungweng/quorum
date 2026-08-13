package cli

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/ui"
)

func settingsOutput(t *testing.T, a *app) string {
	t.Helper()
	var buf bytes.Buffer
	a.out = ui.New(os.Stdout).To(&buf)
	a.printSettings()
	return buf.String()
}

// The value column starts at the same offset in every row. The name column
// used to be a hardcoded 16, which the two longest row names overflowed.
func TestPrintSettingsAlignsEveryValueColumn(t *testing.T) {
	a := testApp(t)
	got := settingsOutput(t, a)

	width := 0
	for _, s := range a.settings() {
		width = max(width, len(s.name))
	}
	for _, s := range a.settings() {
		want := "  " + ui.Pad(s.name, width) + " " + s.value(a.cfg) + "\n"
		if !strings.Contains(got, want) {
			t.Errorf("row %q is not aligned to column %d:\n%s", s.name, width, got)
		}
	}
}

func TestPrintSettingsShowsEveryGroupTitle(t *testing.T) {
	a := testApp(t)
	got := settingsOutput(t, a)
	for _, g := range a.settingGroups() {
		if !strings.Contains(got, "\n"+g.title+"\n") {
			t.Errorf("group title %q is missing:\n%s", g.title, got)
		}
	}
}

// Rows are indexed by name here and in the other settings tests; a duplicate
// name would shadow a row silently.
func TestSettingRowNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range testApp(t).settings() {
		if seen[s.name] {
			t.Errorf("duplicate setting row %q", s.name)
		}
		seen[s.name] = true
	}
}

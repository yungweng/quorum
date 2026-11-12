package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/ui"
)

func settingsOutput(t *testing.T, a *app) string {
	t.Helper()
	var buf bytes.Buffer
	a.out = plainWriter(&buf)
	a.printSettings()
	return buf.String()
}

// The value column starts at the same offset in every row. The name column
// used to be a hardcoded 16, which the two longest row names overflowed.
func TestPrintSettingsAlignsEveryValueColumn(t *testing.T) {
	a := testApp(t)
	got := settingsOutput(t, a)

	var rows []string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "  ") {
			rows = append(rows, line)
		}
	}
	settings := a.settings()
	if len(rows) != len(settings) {
		t.Fatalf("printed %d rows for %d settings:\n%s", len(rows), len(settings), got)
	}
	column := -1
	for i, s := range settings {
		row, value := rows[i], s.value(a.cfg)
		if !strings.HasPrefix(row, "  "+s.name+" ") || !strings.HasSuffix(row, value) {
			t.Fatalf("row %d is %q, want setting %q with value %q", i, row, s.name, value)
		}
		// The value ends the row, so where it starts is the row's width minus its own.
		start := ui.Cells(row) - ui.Cells(value)
		if start <= 2+ui.Cells(s.name) {
			t.Errorf("value of %q starts inside its name:\n%s", s.name, got)
		}
		if column < 0 {
			column = start
		} else if start != column {
			t.Errorf("value of %q starts at column %d, the first row's at %d:\n%s", s.name, start, column, got)
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

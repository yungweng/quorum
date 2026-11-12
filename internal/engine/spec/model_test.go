package spec

import "testing"

// The tag goes on every step line of a babysit run and on the dashboard, so
// what it does with a half-filled Model is not cosmetic. An unset name is the
// fix side, where quorum passes no model at all and never learns which one the
// CLI picked; claiming a name there would be a guess.
func TestModelTagNamesWhatIsKnown(t *testing.T) {
	for _, tc := range []struct {
		m    Model
		want string
	}{
		{Model{Engine: Claude, Name: "opus", Effort: "high"}, "opus/high"},
		{Model{Engine: Claude, Name: "sonnet"}, "sonnet"},
		{Model{Engine: Claude}, "claude default"},
		{Model{Engine: Codex, Effort: "high"}, "codex default/high"},
		{Model{}, "codex default"},
		{Model{Effort: "max"}, "codex default/max"},
	} {
		if got := tc.m.Tag(); got != tc.want {
			t.Errorf("Model%+v.Tag() = %q, want %q", tc.m, got, tc.want)
		}
	}
}

func TestBinsForResolvesTheEngineName(t *testing.T) {
	bins := Bins{Codex: "/bin/codex", Claude: "/bin/claude", Grok: "/bin/grok"}
	for name, want := range map[string]string{
		"":        "/bin/codex",
		Codex:     "/bin/codex",
		Claude:    "/bin/claude",
		Grok:      "/bin/grok",
		"unknown": "/bin/codex",
	} {
		if got := bins.For(name); got != want {
			t.Errorf("For(%q) = %q, want %q", name, got, want)
		}
	}
	if got := (Bins{Codex: "/bin/codex"}).For(Claude); got != "" {
		t.Errorf("missing claude binary = %q, want empty", got)
	}
}

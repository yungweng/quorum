package engine

import (
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/claudecli"
	"github.com/yungweng/quorum/internal/codex"
	"github.com/yungweng/quorum/internal/grokcli"
)

func TestValidAcceptsEmptyCodexClaudeAndGrok(t *testing.T) {
	for _, ok := range []string{"", Codex, Claude, Grok} {
		if !Valid(ok) {
			t.Errorf("%q was rejected", ok)
		}
	}
	for _, bad := range []string{"gpt", "Codex", "claude-code", "grok-build"} {
		if Valid(bad) {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// Engines share a config field but not a set of levels. An empty engine name
// means Codex here for the same reason it does everywhere else.
func TestEffortsAreLookedUpPerEngine(t *testing.T) {
	if !ValidEffort(Codex, "ultra") || !ValidEffort("", "minimal") {
		t.Error("codex lost one of its own effort levels")
	}
	if ValidEffort(Claude, "ultra") || ValidEffort(Claude, "minimal") {
		t.Error("a codex-only effort was accepted for claude")
	}
	if !ValidEffort(Claude, "max") || !ValidEffort(Claude, "") {
		t.Error("claude rejected an effort it accepts")
	}
	if ValidEffort(Grok, "ultra") || ValidEffort(Grok, "max") || ValidEffort(Grok, "xhigh") {
		t.Error("a non-grok effort was accepted for grok")
	}
	if !ValidEffort(Grok, "high") || !ValidEffort(Grok, "") {
		t.Error("grok rejected an effort it accepts")
	}
	if strings.Join(Efforts(Claude), ",") == strings.Join(Efforts(Codex), ",") {
		t.Error("claude and codex reported the same effort levels")
	}
	if strings.Join(Efforts(Grok), ",") == strings.Join(Efforts(Codex), ",") {
		t.Error("grok and codex reported the same effort levels")
	}
}

func TestNewReviewerDefaultsToCodex(t *testing.T) {
	r, err := NewReviewer("", ReviewerOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	opts, ok := r.(codex.Options)
	if !ok {
		t.Fatalf("empty engine built %T, want codex.Options", r)
	}
	if !opts.DisableSerena {
		t.Error("the codex review engine must disable Serena")
	}
	if opts.Bypass {
		t.Error("a review engine carried the sandbox bypass")
	}
}

func TestNewReviewerBuildsClaude(t *testing.T) {
	r, err := NewReviewer(Claude, ReviewerOptions{Model: "sonnet"})
	if err != nil {
		t.Fatal(err)
	}
	opts, ok := r.(claudecli.Options)
	if !ok {
		t.Fatalf("claude engine built %T", r)
	}
	if opts.Bypass {
		t.Error("a review engine carried the permission bypass")
	}
}

func TestNewReviewerBuildsGrok(t *testing.T) {
	r, err := NewReviewer(Grok, ReviewerOptions{Model: "grok-4.5"})
	if err != nil {
		t.Fatal(err)
	}
	opts, ok := r.(grokcli.Options)
	if !ok {
		t.Fatalf("grok engine built %T", r)
	}
	if opts.Bypass {
		t.Error("a review engine carried the permission bypass")
	}
}

func TestNewReviewerRejectsUnknownEngine(t *testing.T) {
	_, err := NewReviewer("gemini", ReviewerOptions{})
	if err == nil || !strings.Contains(err.Error(), "gemini") {
		t.Fatalf("err = %v, want a rejection naming the engine", err)
	}
}

func TestNewFixerCarriesBypass(t *testing.T) {
	f, err := NewFixer(Codex, FixerOptions{Bypass: true})
	if err != nil {
		t.Fatal(err)
	}
	if !f.(codex.Options).Bypass {
		t.Error("codex fixer lost the bypass")
	}
	cf, err := NewFixer(Claude, FixerOptions{Bypass: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cf.(claudecli.Options).Bypass {
		t.Error("claude fixer lost the bypass")
	}
	gf, err := NewFixer(Grok, FixerOptions{Bypass: true})
	if err != nil {
		t.Fatal(err)
	}
	if !gf.(grokcli.Options).Bypass {
		t.Error("grok fixer lost the bypass")
	}
}

func TestNewFixerRejectsUnknownEngine(t *testing.T) {
	if _, err := NewFixer("gemini", FixerOptions{}); err == nil {
		t.Fatal("unknown engine was accepted")
	}
}

// An effort out of the config is dropped rather than refused, and only when
// the resolved engine cannot use it.
func TestKnownEffortDropsOnlyWhatTheEngineCannotUse(t *testing.T) {
	if got := KnownEffort(Claude, "ultra"); got != "" {
		t.Errorf("KnownEffort(claude, ultra) = %q, want it dropped", got)
	}
	if got := KnownEffort(Claude, "xhigh"); got != "xhigh" {
		t.Errorf("KnownEffort(claude, xhigh) = %q, want it kept", got)
	}
	if got := KnownEffort(Grok, "ultra"); got != "" {
		t.Errorf("KnownEffort(grok, ultra) = %q, want it dropped", got)
	}
	if got := KnownEffort(Grok, "high"); got != "high" {
		t.Errorf("KnownEffort(grok, high) = %q, want it kept", got)
	}
	if got := KnownEffort("", "ultra"); got != "ultra" {
		t.Errorf("KnownEffort(codex, ultra) = %q, want it kept", got)
	}
	if got := KnownEffort(Codex, ""); got != "" {
		t.Errorf("KnownEffort(codex, empty) = %q", got)
	}
}

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

package spec

import (
	"strings"
	"testing"
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

func TestReviewDefaults(t *testing.T) {
	eng, model, effort := ReviewDefaults("", "", "")
	if eng != Codex || model != Get(Codex).ReviewModel || effort != Get(Codex).ReviewEffort {
		t.Fatalf("empty input resolved to %s/%s/%s", eng, model, effort)
	}
	eng, model, effort = ReviewDefaults(Claude, "", "high")
	if eng != Claude || model != Get(Claude).ReviewModel {
		t.Fatalf("claude resolved to %s/%s", eng, model)
	}
	if effort != "high" {
		t.Fatalf("an explicit effort was rewritten to %q", effort)
	}
	if _, model, _ = ReviewDefaults(Claude, "opus", ""); model != "opus" {
		t.Fatalf("an explicit model was rewritten to %q", model)
	}
	eng, model, effort = ReviewDefaults(Grok, "", "medium")
	if eng != Grok || model != Get(Grok).ReviewModel {
		t.Fatalf("grok resolved to %s/%s", eng, model)
	}
	if effort != "medium" {
		t.Fatalf("an explicit grok effort was rewritten to %q", effort)
	}
	if _, model, _ = ReviewDefaults(Grok, "custom-model", ""); model != "custom-model" {
		t.Fatalf("an explicit grok model was rewritten to %q", model)
	}
}

func TestChoicesNamesEveryEngine(t *testing.T) {
	if got := Choices(); got != "codex, claude or grok" {
		t.Fatalf("Choices() = %q", got)
	}
}

// Claude answers an effort level it does not know with a warning and its own
// default, so an unvalidated Codex-only level would run the whole panel at the
// wrong setting and never say so.
func TestClaudeEffortsRejectTheCodexOnlyLevels(t *testing.T) {
	for _, level := range []string{"minimal", "ultra", "insane", "MAX"} {
		if ValidEffort(Claude, level) {
			t.Errorf("%q was accepted as a claude effort", level)
		}
	}
	for _, level := range []string{"", "low", "medium", "high", "xhigh", "max"} {
		if !ValidEffort(Claude, level) {
			t.Errorf("%q was rejected as a claude effort", level)
		}
	}
}

// Grok rejects an effort level it does not know, but the accepted set is
// model-menu specific. A codex-only level must not pass validation here.
func TestGrokEffortsRejectUnknownLevels(t *testing.T) {
	for _, level := range []string{"minimal", "ultra", "xhigh", "max", "insane", "HIGH"} {
		if ValidEffort(Grok, level) {
			t.Errorf("%q was accepted as a grok effort", level)
		}
	}
	for _, level := range []string{"", "low", "medium", "high"} {
		if !ValidEffort(Grok, level) {
			t.Errorf("%q was rejected as a grok effort", level)
		}
	}
}

// Codex silently ignores an unknown effort instead of failing, so a typo would
// run every reviewer at the wrong setting and only show up in the bill.
func TestCodexEffortValidation(t *testing.T) {
	for _, ok := range []string{"", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"} {
		if !ValidEffort(Codex, ok) {
			t.Errorf("%q was rejected", ok)
		}
	}
	for _, bad := range []string{"maximum", "Medium", "hi", "0"} {
		if ValidEffort(Codex, bad) {
			t.Errorf("%q was accepted", bad)
		}
	}
}

package engine

import (
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/claudecli"
	"github.com/yungweng/quorum/internal/codex"
)

func TestValidAcceptsEmptyCodexAndClaude(t *testing.T) {
	for _, ok := range []string{"", Codex, Claude} {
		if !Valid(ok) {
			t.Errorf("%q was rejected", ok)
		}
	}
	for _, bad := range []string{"gpt", "Codex", "claude-code"} {
		if Valid(bad) {
			t.Errorf("%q was accepted", bad)
		}
	}
}

// The two engines share a config field but not a set of levels. An empty
// engine name means Codex here for the same reason it does everywhere else.
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
	if strings.Join(Efforts(Claude), ",") == strings.Join(Efforts(Codex), ",") {
		t.Error("both engines reported the same effort levels")
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
}

func TestNewFixerRejectsUnknownEngine(t *testing.T) {
	if _, err := NewFixer("gemini", FixerOptions{}); err == nil {
		t.Fatal("unknown engine was accepted")
	}
}

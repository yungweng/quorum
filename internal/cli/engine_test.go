package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/engine/spec"
)

func TestEngineBinaryAcceptsAFoundEngine(t *testing.T) {
	bins := spec.Bins{spec.Codex: "/bin/codex", spec.Claude: "/bin/claude", spec.Grok: "/bin/grok"}
	for _, eng := range []string{"", spec.Codex, spec.Claude, spec.Grok} {
		if err := engineBinary(eng, bins, "--engine"); err != nil {
			t.Errorf("%q: err = %v", eng, err)
		}
	}
}

func TestEngineBinaryErrorsNameTheFlag(t *testing.T) {
	codexOnly := spec.Bins{spec.Codex: "/bin/codex"}
	err := engineBinary(spec.Claude, codexOnly, "--engine/REVIEW_ENGINE")
	if err == nil || !strings.Contains(err.Error(), "--engine/REVIEW_ENGINE") {
		t.Fatalf("missing claude binary: err = %v, want a hint naming the flag", err)
	}
	err = engineBinary(spec.Grok, codexOnly, "--engine/FIX_ENGINE")
	if err == nil || !strings.Contains(err.Error(), "--engine/FIX_ENGINE") {
		t.Fatalf("missing grok binary: err = %v, want a hint naming the flag", err)
	}
}

func TestEngineBinaryNamesWhatToInstall(t *testing.T) {
	for eng, want := range map[string]string{
		spec.Claude: "engine claude is selected (--engine) but no claude binary was found on PATH; install Claude Code or switch back to codex",
		spec.Grok:   "engine grok is selected (--engine) but no grok binary was found on PATH; install the Grok CLI or switch back to codex",
	} {
		if err := engineBinary(eng, spec.Bins{spec.Codex: "/bin/codex"}, "--engine"); err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", eng, err, want)
		}
	}
}

// An explicit model belongs to the engine it was typed under. Switching the
// engine must reset model and effort, or claude would be handed a GPT model.
func TestEngineSwitchResetsModelAndEffort(t *testing.T) {
	opts := engineOptions(
		func(c *config.Config) *string { return &c.ReviewEngine },
		func(c *config.Config) *string { return &c.ReviewModel },
		func(c *config.Config) *string { return &c.ReviewEffort },
	)
	cfg := config.Default()
	cfg.ReviewModel = "gpt-5.6-terra"
	cfg.ReviewEffort = "medium"

	var claude option
	for _, o := range opts {
		if o.label == "claude" {
			claude = o
		}
	}
	claude.apply(&cfg)
	if cfg.ReviewEngine != spec.Claude {
		t.Fatalf("engine = %q", cfg.ReviewEngine)
	}
	if cfg.ReviewModel != "" || cfg.ReviewEffort != "" {
		t.Fatalf("switching engines kept model/effort %q/%q", cfg.ReviewModel, cfg.ReviewEffort)
	}

	// Re-picking the same engine keeps an explicitly chosen model.
	cfg.ReviewModel = "opus"
	claude.apply(&cfg)
	if cfg.ReviewModel != "opus" {
		t.Fatalf("re-picking the same engine reset the model to %q", cfg.ReviewModel)
	}
}

// codex is an engine like the others: needed when selected, and only then.
func TestEngineBinaryNeedsCodexOnlyWhenSelected(t *testing.T) {
	claudeOnly := spec.Bins{spec.Claude: "/bin/claude"}
	if err := engineBinary(spec.Claude, claudeOnly, "REVIEW_ENGINE"); err != nil {
		t.Errorf("claude without codex installed: %v", err)
	}
	want := "engine codex is selected (REVIEW_ENGINE) but no codex binary was found on PATH; install " + spec.Get(spec.Codex).Install
	if err := engineBinary(spec.Codex, claudeOnly, "REVIEW_ENGINE"); err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestAgentToolsRequireTheConfiguredEnginesOnly(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"gh", "git", "claude", "direnv"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	isolateHome(t)
	t.Setenv("PATH", dir)

	a := &app{cfg: config.Default()}
	if _, err := a.findTools(); err != nil {
		t.Fatalf("findTools needed more than gh and git: %v", err)
	}
	if _, err := a.agentTools(); err == nil || !strings.Contains(err.Error(), "engine codex is selected (REVIEW_ENGINE)") {
		t.Fatalf("the default config ran without codex: %v", err)
	}
	a.cfg.ReviewEngine = spec.Claude
	if _, err := a.agentTools(); err != nil {
		t.Fatalf("a claude review needed codex: %v", err)
	}
	// The fix engine only matters once the agent runs fix loops.
	a.cfg.AgentAction = config.ActionBabysit
	if _, err := a.agentTools(); err == nil || !strings.Contains(err.Error(), "FIX_ENGINE") {
		t.Fatalf("a codex fix loop ran without codex: %v", err)
	}
}

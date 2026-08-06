package cli

import (
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/config"
)

func TestEngineBinaryResolvesClaudeOrErrors(t *testing.T) {
	tls := tools{Codex: "/bin/codex", Claude: "/bin/claude"}
	got, err := engineBinary(config.EngineClaude, tls, "--engine")
	if err != nil || got != "/bin/claude" {
		t.Fatalf("claude = (%q, %v)", got, err)
	}
	got, err = engineBinary("", tls, "--engine")
	if err != nil || got != "/bin/codex" {
		t.Fatalf("empty engine = (%q, %v), want the codex binary", got, err)
	}
	_, err = engineBinary(config.EngineClaude, tools{Codex: "/bin/codex"}, "--engine/REVIEW_ENGINE")
	if err == nil || !strings.Contains(err.Error(), "--engine/REVIEW_ENGINE") {
		t.Fatalf("missing claude binary: err = %v, want a hint naming the flag", err)
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
	if cfg.ReviewEngine != config.EngineClaude {
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

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Empty model and effort mean "the engine's own default": a hardcoded codex
// model here would be fed to claude the moment REVIEW_ENGINE flips.
func TestDefaultEngineIsCodexWithEmptyModelAndEffort(t *testing.T) {
	cfg := Default()
	if cfg.ReviewEngine != EngineCodex || cfg.FixEngine != EngineCodex {
		t.Fatalf("default engines = %q/%q", cfg.ReviewEngine, cfg.FixEngine)
	}
	if cfg.ReviewModel != "" || cfg.ReviewEffort != "" {
		t.Fatalf("default review model/effort = %q/%q, want empty", cfg.ReviewModel, cfg.ReviewEffort)
	}
}

func TestEngineKeysAcceptOnlyKnownEngines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	body := "REVIEW_ENGINE=\"claude\"\nFIX_ENGINE=\"gemini\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReviewEngine != EngineClaude {
		t.Errorf("REVIEW_ENGINE=claude was not applied: %q", cfg.ReviewEngine)
	}
	if cfg.FixEngine != EngineCodex {
		t.Errorf("an unknown FIX_ENGINE replaced the default: %q", cfg.FixEngine)
	}
}

func TestEngineKeysSurviveARoundTrip(t *testing.T) {
	want := Default()
	want.ReviewEngine = EngineClaude
	want.FixEngine = EngineClaude
	path := filepath.Join(t.TempDir(), "config")
	if err := want.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.ReviewEngine != EngineClaude || got.FixEngine != EngineClaude {
		t.Fatalf("round trip lost the engines: %q/%q", got.ReviewEngine, got.FixEngine)
	}
}

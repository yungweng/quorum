package runner

import (
	"reflect"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/engine/spec"
	"github.com/yungweng/quorum/internal/loop"
	"github.com/yungweng/quorum/internal/paths"
	"github.com/yungweng/quorum/internal/review"
)

// optionsFixture is a configuration that differs from config.Default in every
// key the option builders read, so a key that a builder drops or crosses with
// another one shows up as a mismatch.
func optionsFixture() (config.Config, paths.P, Tools) {
	cfg := config.Default()
	cfg.Reviewers = 3
	cfg.ReviewEngine = spec.Claude
	cfg.ReviewModel = "review-model"
	cfg.ReviewEffort = "high"
	cfg.ReviewTimeout = 7 * time.Minute
	cfg.Post = false
	cfg.FixEngine = spec.Grok
	cfg.FixModel = "fix-model"
	cfg.FixEffort = "low"
	cfg.MaxIter = 4
	cfg.MaxCIFixes = 5
	cfg.FixTimeout = 11 * time.Minute
	cfg.DivergenceScan = true
	cfg.DivergenceEscalateTo = []string{"example-user"}
	cfg.Sandboxed = true
	cfg.ResolveConflicts = false
	cfg.BaseUpdate = config.BaseUpdateConflicts
	cfg.FixSuggestions = false
	cfg.LoopMode = config.LoopOnline
	p := paths.P{
		ReviewRuns:  "/cache/review-runs",
		BabysitRuns: "/cache/babysit-runs",
		DepsCache:   "/cache/deps",
	}
	t := Tools{
		GH: "/bin/gh", Git: "/bin/git", Direnv: "/bin/direnv",
		Engines: spec.Bins{spec.Codex: "/bin/codex", spec.Claude: "/bin/claude", spec.Grok: "/bin/grok"},
	}
	return cfg, p, t
}

// wantReviewOptions is what ReviewOptions builds from optionsFixture.
func wantReviewOptions(tools Tools) review.Options {
	return review.Options{
		Repo:          "acme/api",
		Number:        42,
		RepoRoot:      "/src/api",
		Runs:          3,
		Engine:        spec.Claude,
		Model:         "review-model",
		Effort:        "high",
		ReviewTimeout: 7 * time.Minute,
		Post:          false,
		UseDirenv:     true,
		RunsDir:       "/cache/review-runs",
		RunRoots:      []string{"/cache/review-runs", "/cache/babysit-runs"},
		DepsDir:       "/cache/deps",
		Bins:          tools.Engines,
		DirenvBin:     "/bin/direnv",
	}
}

func TestReviewOptionsMapsEveryConfigKey(t *testing.T) {
	cfg, p, tools := optionsFixture()
	got := ReviewOptions(cfg, p, tools, "acme/api", "/src/api", 42)
	want := wantReviewOptions(tools)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ReviewOptions =\n%+v\nwant\n%+v", got, want)
	}

	tools.Direnv = ""
	if got := ReviewOptions(cfg, p, tools, "acme/api", "/src/api", 42); got.UseDirenv {
		t.Fatal("UseDirenv is set without a direnv binary")
	}
}

func TestLoopOptionsMapsEveryConfigKey(t *testing.T) {
	cfg, p, tools := optionsFixture()
	got := LoopOptions(cfg, p, tools, "acme/api", "/src/api", 42)
	want := loop.Options{
		Repo:                 "acme/api",
		RepoRoot:             "/src/api",
		Number:               42,
		Engine:               spec.Grok,
		Model:                "fix-model",
		Effort:               "low",
		Review:               wantReviewOptions(tools),
		MaxIter:              4,
		MaxCIFixes:           5,
		FixTimeout:           11 * time.Minute,
		DivergenceScan:       true,
		DivergenceEscalateTo: []string{"example-user"},
		DivergenceTimeout:    7 * time.Minute,
		Post:                 false,
		ResolveConflicts:     false,
		BaseUpdateOnConflict: true,
		FixSuggestions:       false,
		Offline:              false,
		Bypass:               false,
		UseDirenv:            true,
		RunsDir:              "/cache/babysit-runs",
		DepsDir:              "/cache/deps",
		Bins:                 tools.Engines,
		DirenvBin:            "/bin/direnv",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LoopOptions =\n%+v\nwant\n%+v", got, want)
	}

	// The escalation list is copied, so a run cannot write into the config.
	got.DivergenceEscalateTo[0] = "changed"
	if cfg.DivergenceEscalateTo[0] != "example-user" {
		t.Fatal("LoopOptions shares DivergenceEscalateTo with the config")
	}

	tools.Direnv = ""
	if got := LoopOptions(cfg, p, tools, "acme/api", "/src/api", 42); got.UseDirenv {
		t.Fatal("UseDirenv is set without a direnv binary")
	}
}

package runner

import (
	"slices"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/engine/spec"
	"github.com/yungweng/quorum/internal/loop"
	"github.com/yungweng/quorum/internal/paths"
	"github.com/yungweng/quorum/internal/review"
)

// Tools is the set of external binaries quorum drives, resolved once.
type Tools struct {
	GH  string
	Git string
	// Direnv is optional: repositories without an .envrc never need it.
	Direnv string
	// Engines holds every engine binary that was found. Codex is required;
	// the others are only needed when an engine setting selects them.
	Engines spec.Bins
}

// ReviewOptions maps the configuration, paths and tools onto a review run.
// It sets only what follows from those; flags, rules and the unattended
// overrides are the caller's. Concurrency stays 0, every pass at once: a
// review that trickles through its passes is a review you wait for.
func ReviewOptions(cfg config.Config, p paths.P, t Tools, repo, repoRoot string, number int) review.Options {
	return review.Options{
		Repo:          repo,
		Number:        number,
		RepoRoot:      repoRoot,
		Runs:          cfg.Reviewers,
		Engine:        cfg.ReviewEngine,
		Model:         cfg.ReviewModel,
		Effort:        cfg.ReviewEffort,
		ReviewTimeout: cfg.ReviewTimeout,
		Post:          cfg.Post,
		UseDirenv:     t.Direnv != "",
		RunsDir:       p.ReviewRuns,
		RunRoots:      p.RunRoots(),
		DepsDir:       p.DepsCache,
		Bins:          t.Engines,
		DirenvBin:     t.Direnv,
	}
}

// LoopOptions maps the configuration, paths and tools onto a fix-loop run.
// It sets only what follows from those; flags, rules, drafts, the test
// command and the unattended overrides are the caller's.
func LoopOptions(cfg config.Config, p paths.P, t Tools, repo, repoRoot string, number int) loop.Options {
	return loop.Options{
		Repo:                 repo,
		RepoRoot:             repoRoot,
		Number:               number,
		Engine:               cfg.FixEngine,
		Model:                cfg.FixModel,
		Effort:               cfg.FixEffort,
		Review:               ReviewOptions(cfg, p, t, repo, repoRoot, number),
		MaxIter:              cfg.MaxIter,
		MaxCIFixes:           cfg.MaxCIFixes,
		FixTimeout:           cfg.FixTimeout,
		DivergenceScan:       cfg.DivergenceScan,
		DivergenceEscalateTo: slices.Clone(cfg.DivergenceEscalateTo),
		DivergenceTimeout:    cfg.ReviewTimeout,
		Post:                 cfg.Post,
		ResolveConflicts:     cfg.ResolveConflicts,
		BaseUpdateOnConflict: cfg.BaseUpdate == config.BaseUpdateConflicts,
		FixSuggestions:       cfg.FixSuggestions,
		Offline:              cfg.LoopMode != config.LoopOnline,
		Bypass:               !cfg.Sandboxed,
		UseDirenv:            t.Direnv != "",
		RunsDir:              p.BabysitRuns,
		DepsDir:              p.DepsCache,
		Bins:                 t.Engines,
		DirenvBin:            t.Direnv,
	}
}

// Package engine is the seam between the pipeline and the CLIs that can
// drive it. internal/review and internal/loop talk to these interfaces;
// internal/codex, internal/claudecli and internal/grokcli satisfy them
// structurally, without importing this package.
//
// The split into Reviewer and Fixer is deliberate: NewReviewer takes no
// bypass, so a review-side engine with the sandbox bypass is not a convention
// violation, it does not compile.
package engine

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/claudecli"
	"github.com/yungweng/quorum/internal/codex"
	"github.com/yungweng/quorum/internal/engine/spec"
	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/grokcli"
)

// Reviewer runs the read-only, non-resumable passes of a review. rules is the
// repository's user-local review rules, or empty when the repository has none;
// each adapter injects them into its reviewer prompt.
type Reviewer interface {
	Review(ctx context.Context, env envexec.Env, timeout time.Duration, baseRef, rules, outFile string, log io.Writer) error
	// Prompt runs one read-only, ephemeral pass: prompt is the instruction,
	// stdin (when not nil) the material behind it, and the final answer goes
	// to outFile. Aggregation, verification, the PR description and the
	// divergence analysis are all this pass with different prompts.
	Prompt(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error
}

// NewReviewer builds the review-side engine m names. There is deliberately no
// bypass here. m.Effort is one of spec.Efforts(m.Engine); empty leaves the
// engine's default.
func NewReviewer(m spec.Model, bins spec.Bins) (Reviewer, error) {
	bin := binary(m.Engine, bins)
	switch m.Engine {
	case "", spec.Codex:
		return codex.Options{Bin: bin, Model: m.Name, Effort: m.Effort, DisableSerena: true}, nil
	case spec.Claude:
		return claudecli.Options{Bin: bin, Model: m.Name, Effort: m.Effort}, nil
	case spec.Grok:
		return grokcli.Options{Bin: bin, Model: m.Name, Effort: m.Effort}, nil
	}
	return nil, unknown(m.Engine)
}

// Fixer runs the resumable fix session.
type Fixer interface {
	Exec(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, log io.Writer) (string, error)
	Resume(ctx context.Context, env envexec.Env, timeout time.Duration, sessionID, prompt, outFile string, log io.Writer) error
}

// NewFixer builds the fix-session engine m names. bypass lifts the sandbox so
// the session can run tests, use gh and push unattended.
func NewFixer(m spec.Model, bins spec.Bins, bypass bool) (Fixer, error) {
	bin := binary(m.Engine, bins)
	switch m.Engine {
	case "", spec.Codex:
		return codex.Options{Bin: bin, Model: m.Name, Effort: m.Effort, Bypass: bypass}, nil
	case spec.Claude:
		return claudecli.Options{Bin: bin, Model: m.Name, Effort: m.Effort, Bypass: bypass}, nil
	case spec.Grok:
		return grokcli.Options{Bin: bin, Model: m.Name, Effort: m.Effort, Bypass: bypass}, nil
	}
	return nil, unknown(m.Engine)
}

// binary is the engine's resolved binary, or its plain name for PATH to find
// when none was resolved.
func binary(name string, bins spec.Bins) string {
	if bin := bins.For(name); bin != "" {
		return bin
	}
	return spec.Get(name).Bin
}

func unknown(name string) error {
	return fmt.Errorf("unknown engine %q (valid: %s)", name, strings.Join(spec.Names(), ", "))
}

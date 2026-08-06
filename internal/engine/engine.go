// Package engine is the seam between the pipeline and the two CLIs that can
// drive it. internal/review and internal/loop talk to these interfaces;
// internal/codex and internal/claudecli satisfy them structurally, without
// importing this package.
//
// The split into Reviewer and Fixer is deliberate: ReviewerOptions has no
// Bypass field, so a review-side engine with the sandbox bypass is not a
// convention violation, it does not compile.
package engine

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/yungweng/quorum/internal/claudecli"
	"github.com/yungweng/quorum/internal/codex"
	"github.com/yungweng/quorum/internal/envexec"
)

// The recognised engine names. Empty means Codex, which keeps every config
// and option struct from before the seam meaning what it always meant.
const (
	Codex  = "codex"
	Claude = "claude"
)

// Valid reports whether name is a recognised engine or empty.
func Valid(name string) bool {
	return name == "" || name == Codex || name == Claude
}

// Efforts returns the reasoning-effort values name accepts. The two sets are
// not the same, and neither CLI fails on a level it does not know: both take
// the default instead, so the only place an engine-specific typo can still be
// caught is here, before anything is spawned.
func Efforts(name string) []string {
	if name == Claude {
		return claudecli.Efforts
	}
	return codex.Efforts
}

// ValidEffort reports whether effort is one of Efforts(name). The empty string
// is allowed for either engine and means "leave its own default alone".
func ValidEffort(name, effort string) bool {
	if name == Claude {
		return claudecli.ValidEffort(effort)
	}
	return codex.ValidEffort(effort)
}

// SessionRef is the opaque handle Exec returns and Resume takes back. It is
// an alias, not a defined type, so the adapters can keep returning plain
// strings and still satisfy Fixer without importing this package.
type SessionRef = string

// Reviewer runs the read-only, non-resumable passes of a review.
type Reviewer interface {
	Review(ctx context.Context, env envexec.Env, timeout time.Duration, baseRef, outFile string, log io.Writer) error
	Aggregate(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error
	Verify(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error
	DescribePR(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, stdin io.Reader, log io.Writer) error
}

// ReviewerOptions selects the model settings for a review-side engine. There
// is deliberately no Bypass here.
type ReviewerOptions struct {
	Bin    string
	Model  string
	Effort string // one of Efforts(name); empty leaves the engine's default
}

// NewReviewer builds the review-side engine for name.
func NewReviewer(name string, o ReviewerOptions) (Reviewer, error) {
	switch name {
	case "", Codex:
		return codex.Options{Bin: o.Bin, Model: o.Model, Effort: o.Effort, DisableSerena: true}, nil
	case Claude:
		return claudecli.Options{Bin: o.Bin, Model: o.Model, Effort: o.Effort}, nil
	}
	return nil, fmt.Errorf("unknown engine %q (valid: %s, %s)", name, Codex, Claude)
}

// Fixer runs the resumable fix session.
type Fixer interface {
	Exec(ctx context.Context, env envexec.Env, timeout time.Duration, prompt, outFile string, log io.Writer) (SessionRef, error)
	Resume(ctx context.Context, env envexec.Env, timeout time.Duration, ref SessionRef, prompt, outFile string, log io.Writer) error
}

// FixerOptions selects the model settings for a fix-session engine. Bypass
// lifts the sandbox so the session can run tests, use gh and push unattended.
type FixerOptions struct {
	Bin    string
	Model  string
	Effort string // one of Efforts(name); empty leaves the engine's default
	Bypass bool
}

// NewFixer builds the fix-session engine for name.
func NewFixer(name string, o FixerOptions) (Fixer, error) {
	switch name {
	case "", Codex:
		return codex.Options{Bin: o.Bin, Model: o.Model, Effort: o.Effort, Bypass: o.Bypass}, nil
	case Claude:
		return claudecli.Options{Bin: o.Bin, Model: o.Model, Effort: o.Effort, Bypass: o.Bypass}, nil
	}
	return nil, fmt.Errorf("unknown engine %q (valid: %s, %s)", name, Codex, Claude)
}

// Package engine is the seam between the pipeline and the CLIs that can
// drive it. internal/review and internal/loop talk to these interfaces;
// internal/codex, internal/claudecli and internal/grokcli satisfy them
// structurally, without importing this package.
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
	"github.com/yungweng/quorum/internal/grokcli"
)

// The recognised engine names. Empty means Codex, which keeps every config
// and option struct from before the seam meaning what it always meant.
const (
	Codex  = "codex"
	Claude = "claude"
	Grok   = "grok"
)

// Valid reports whether name is a recognised engine or empty.
func Valid(name string) bool {
	return name == "" || name == Codex || name == Claude || name == Grok
}

// Efforts returns the reasoning-effort values name accepts. The sets are not
// the same, and some CLIs fall back to a default on an unknown level instead
// of failing, so the only place an engine-specific typo can still be caught
// is here, before anything is spawned.
func Efforts(name string) []string {
	switch name {
	case Claude:
		return claudecli.Efforts
	case Grok:
		return grokcli.Efforts
	default:
		return codex.Efforts
	}
}

// ValidEffort reports whether effort is one of Efforts(name). The empty string
// is allowed for any engine and means "leave its own default alone".
func ValidEffort(name, effort string) bool {
	switch name {
	case Claude:
		return claudecli.ValidEffort(effort)
	case Grok:
		return grokcli.ValidEffort(effort)
	default:
		return codex.ValidEffort(effort)
	}
}

// KnownEffort returns effort when name accepts it and the empty string, the
// engine's own default, when it does not. It is for stored values, not typed
// ones: the levels became engine-specific only after quorum's own config
// picker could already write a codex level next to REVIEW_ENGINE="claude", and
// an engine flag can move a run to the other engine's set at any time.
// Refusing such a pair would stop every run on a file the tool wrote itself,
// including the unattended agent's, where nobody reads the error. An effort
// typed on the command line does not go through here; it is validated and
// fails the run.
func KnownEffort(name, effort string) string {
	if ValidEffort(name, effort) {
		return effort
	}
	return ""
}

// Model is the engine, model and effort one call runs on. It lives here
// because every layer that reports a step - the pipeline, the reviewer panel,
// the dashboard, the agent's log - can reach this package, and a single type
// keeps them from formatting the same three strings four different ways.
type Model struct {
	Engine string `json:"engine,omitempty"`
	Name   string `json:"model,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// Tag is the short form a step line carries: "opus/high".
//
// An unset name is the engine's own choice, which has no name here: the fix
// side is handed to the CLI with no -m at all, so quorum knows what it asked
// for and not what the CLI picked. Saying "codex default" is the whole truth
// available. An unset effort is left off rather than guessed at.
func (m Model) Tag() string {
	name := m.Name
	if name == "" {
		eng := m.Engine
		if eng == "" {
			eng = Codex
		}
		name = eng + " default"
	}
	if m.Effort == "" {
		return name
	}
	return name + "/" + m.Effort
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
	case Grok:
		return grokcli.Options{Bin: o.Bin, Model: o.Model, Effort: o.Effort}, nil
	}
	return nil, fmt.Errorf("unknown engine %q (valid: %s, %s, %s)", name, Codex, Claude, Grok)
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
	case Grok:
		return grokcli.Options{Bin: o.Bin, Model: o.Model, Effort: o.Effort, Bypass: o.Bypass}, nil
	}
	return nil, fmt.Errorf("unknown engine %q (valid: %s, %s, %s)", name, Codex, Claude, Grok)
}

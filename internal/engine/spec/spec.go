// Package spec is the table of engines quorum can drive: their names, the
// binaries they run, how the settings UI names them, the reasoning-effort
// levels each accepts and the defaults a review-style pass uses, and the Model
// and Bins values that carry a choice of engine through the layers. It imports
// nothing from this module, so config, the CLI and the pipeline can all read
// it without pulling in the adapters.
package spec

import (
	"slices"
	"strings"
)

// The recognised engine names. Empty means Codex, which keeps every config
// and option struct from before the engine seam meaning what it always meant.
const (
	Codex  = "codex"
	Claude = "claude"
	Grok   = "grok"
)

// The default review models the hints quote as well.
const (
	codexReviewModel = "gpt-5.6-luna"
	grokReviewModel  = "grok-4.5"
)

// Engine is one row of the table.
type Engine struct {
	Name string
	Bin  string // the binary looked up on PATH

	Desc    string // the settings row: "claude (Claude Code)"
	Label   string // the engine picker's detail line
	Install string // what to install, for a missing binary: "Claude Code"
	// ModelHint gives the model prompt examples this engine accepts. The name
	// spaces have nothing in common, and a model typed for the wrong engine
	// is only found out when the run fails.
	ModelHint string

	// Efforts is the set of reasoning-effort values the CLI accepts. The sets
	// are not the same, and some CLIs fall back to a default on an unknown
	// level instead of failing, so the only place an engine-specific typo can
	// still be caught is here, before anything is spawned.
	Efforts []string

	// ReviewModel and ReviewEffort are quorum's own defaults for any
	// review-style pass. Empty leaves the CLI's own default.
	ReviewModel  string
	ReviewEffort string
}

// All is the table, in the order the settings UI and doctor list it.
var All = []Engine{
	{
		Name: Codex, Bin: "codex",
		Desc: "codex", Label: "OpenAI's Codex CLI", Install: "codex",
		ModelHint: "a Codex model name such as " + codexReviewModel,
		// Codex silently ignores an unknown effort instead of failing, so a
		// typo would quietly run every reviewer at the wrong setting and only
		// show up in the bill.
		Efforts:     []string{"minimal", "low", "medium", "high", "xhigh", "max", "ultra"},
		ReviewModel: codexReviewModel, ReviewEffort: "max",
	},
	{
		Name: Claude, Bin: "claude",
		Desc: "claude (Claude Code)", Label: "Anthropic's Claude Code CLI", Install: "Claude Code",
		ModelHint: "an alias such as opus, sonnet or fable, or a full name such as claude-opus-5",
		// Read off `claude --help` (checked against 2.1.223, which documents
		// --effort as "low, medium, high, xhigh, max"). Claude has neither
		// "minimal" nor "ultra", and it answers an unknown level with a
		// warning on stderr and the default effort instead of an error.
		Efforts:     []string{"low", "medium", "high", "xhigh", "max"},
		ReviewModel: "sonnet",
	},
	{
		Name: Grok, Bin: "grok",
		Desc: "grok (Grok CLI)", Label: "xAI's Grok CLI", Install: "the Grok CLI",
		ModelHint: "a Grok model name such as " + grokReviewModel,
		// The Grok CLI's default model menu (checked against 1.0.0). Unknown
		// levels fail at the CLI with a JSON error; validating here keeps a
		// typo from starting a full panel.
		Efforts:     []string{"low", "medium", "high"},
		ReviewModel: grokReviewModel,
	},
}

// lookup returns the row for name. The empty name is Codex.
func lookup(name string) (Engine, bool) {
	if name == "" {
		name = Codex
	}
	for _, e := range All {
		if e.Name == name {
			return e, true
		}
	}
	return Engine{}, false
}

// Get returns the row for name, and Codex for the empty name or one the table
// does not know.
func Get(name string) Engine {
	if e, ok := lookup(name); ok {
		return e
	}
	return All[0]
}

// Valid reports whether name is a recognised engine or empty.
func Valid(name string) bool {
	_, ok := lookup(name)
	return ok
}

func Names() []string {
	names := make([]string, len(All))
	for i, e := range All {
		names[i] = e.Name
	}
	return names
}

// Choices names every engine for an error message: "codex, claude or grok".
func Choices() string {
	names := Names()
	last := len(names) - 1
	return strings.Join(names[:last], ", ") + " or " + names[last]
}

func Efforts(name string) []string {
	return Get(name).Efforts
}

// ValidEffort reports whether effort is one of Efforts(name). The empty string
// is allowed for any engine and means "leave its own default alone".
func ValidEffort(name, effort string) bool {
	return effort == "" || slices.Contains(Efforts(name), effort)
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

// ReviewDefaults resolves engine, model and effort for any review-style pass
// (reviewer, aggregator, verifier, description, divergence analysis): an
// empty engine means Codex, and an engine's opinionated defaults apply only
// when that engine is the resolved one, so a Claude or Grok run is never
// handed a GPT model name. A name the table does not know is returned
// unchanged, for validation to reject.
func ReviewDefaults(eng, model, effort string) (string, string, string) {
	if eng == "" {
		eng = Codex
	}
	if e, ok := lookup(eng); ok {
		if model == "" {
			model = e.ReviewModel
		}
		if effort == "" {
			effort = e.ReviewEffort
		}
	}
	return eng, model, effort
}

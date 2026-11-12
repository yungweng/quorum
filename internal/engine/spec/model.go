package spec

// Model is the engine, model and effort one call runs on. It lives in this
// leaf package because every layer that reports a step - the pipeline, the
// reviewer panel, the dashboard, the agent's log - can reach it, and a single
// type keeps them from formatting the same three strings four different ways.
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

// Bins holds the resolved binary of each engine, keyed by its spec name. An
// engine whose binary was not found has no entry.
type Bins map[string]string

// For returns the binary for the engine name. The empty name, and one the
// table does not know, is Codex, the way Get resolves it.
func (b Bins) For(name string) string {
	return b[Get(name).Name]
}

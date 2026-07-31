// Package paths resolves every file quorum reads or writes.
//
// The three tools quorum replaces kept four separate trees: prbot's config,
// state and clones, pr-codex-review's run cache with the shared dependency
// trees inside it, and babysit's run cache. Everything now hangs off one
// config directory, one state directory and one cache directory, which is what
// makes a single `quorum gc` able to reason about the whole footprint.
package paths

import (
	"os"
	"path/filepath"
)

// P holds the resolved locations for one process.
type P struct {
	Config       string // the config file itself
	StateDir     string
	StateFile    string
	PRStatesFile string // cached GitHub state for dashboard pull requests
	HistoryFile  string // the log of finished runs
	Log          string
	RunningDir   string
	ManualDir    string
	RunLogDir    string
	CloneDir     string

	CacheRoot   string // everything below is under here
	ReviewRuns  string // one directory per review run
	BabysitRuns string // one directory per babysit run
	DepsCache   string // shared dependency trees, keyed by lock file hash

	Plist string
}

// PlistLabel is the launchd job name. Changing it orphans installed agents,
// which is why `quorum install` unloads the old prbot label as well.
const PlistLabel = "io.github.quorum"

// LegacyPlistLabel is the label prbot installed under. Kept so an upgrade can
// unload the old agent instead of leaving two pollers running.
const LegacyPlistLabel = "io.github.prbot"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Resolve honours QUORUM_CONFIG, QUORUM_STATE_DIR and QUORUM_CLONE_DIR over the
// XDG defaults. Where a quorum path does not exist yet but the prbot one does,
// the old location is used, so an upgrade keeps its state instead of starting
// from scratch. Writes always go to the new location.
func Resolve() P {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	configHome := env("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	stateHome := env("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	cacheHome := env("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	stateDir := env("QUORUM_STATE_DIR", filepath.Join(stateHome, "quorum"))
	cacheRoot := filepath.Join(cacheHome, "quorum")

	p := P{
		Config:       env("QUORUM_CONFIG", filepath.Join(configHome, "quorum", "config")),
		StateDir:     stateDir,
		StateFile:    filepath.Join(stateDir, "state.json"),
		PRStatesFile: filepath.Join(stateDir, "pr-states.json"),
		HistoryFile:  filepath.Join(stateDir, "history.jsonl"),
		Log:          filepath.Join(stateDir, "quorum.log"),
		RunningDir:   filepath.Join(stateDir, "running"),
		ManualDir:    filepath.Join(stateDir, "manual-reviews"),
		RunLogDir:    filepath.Join(stateDir, "runs"),
		CloneDir:     env("QUORUM_CLONE_DIR", filepath.Join(cacheRoot, "repos")),

		CacheRoot: cacheRoot,
		// Run directories and the shared dependency cache are siblings. In
		// pr-codex-review the deps tree lived inside the run cache, so the run
		// GC had to skip over it by name; as a sibling it simply cannot be
		// mistaken for a run.
		ReviewRuns:  filepath.Join(cacheRoot, "reviews"),
		BabysitRuns: filepath.Join(cacheRoot, "babysit"),
		DepsCache:   filepath.Join(cacheRoot, "deps"),

		Plist: filepath.Join(home, "Library", "LaunchAgents", PlistLabel+".plist"),
	}

	// Fall back to prbot's locations while quorum has none of its own. Only
	// reads land there; the first save writes to the new path.
	if !exists(p.Config) {
		if legacy := filepath.Join(configHome, "prbot", "config"); exists(legacy) {
			p.Config = legacy
		}
	}
	if !exists(p.StateFile) {
		legacyState := filepath.Join(stateHome, "prbot")
		if exists(filepath.Join(legacyState, "state.json")) {
			p.StateFile = filepath.Join(legacyState, "state.json")
		}
	}

	return p
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// EnsureDirs creates everything a run needs. Callers that only read state can
// skip it; poll, review and babysit cannot.
func (p P) EnsureDirs() error {
	for _, d := range []string{
		p.StateDir, p.RunningDir, p.ManualDir, p.RunLogDir, p.CloneDir,
		p.ReviewRuns, p.BabysitRuns, p.DepsCache,
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

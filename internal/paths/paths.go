// Package paths resolves every file quorum reads or writes.
//
// Everything hangs off one config directory, one state directory and one cache
// directory, which is what makes a single `quorum gc` able to reason about the
// whole footprint.
package paths

import (
	"os"
	"path/filepath"

	"github.com/yungweng/quorum/internal/atomicfile"
)

// P holds the resolved locations for one process.
type P struct {
	Config       string // the config file itself
	RulesDir     string // per-repo review rules: <RulesDir>/<owner>/<repo>.md
	TestCmdDir   string // per-repo test commands: <TestCmdDir>/<owner>/<repo>
	StateDir     string
	StateFile    string
	PRStatesFile string // cached GitHub state for dashboard pull requests
	HistoryFile  string // the log of finished runs
	Log          string
	RunningDir   string
	ManualDir    string
	RunLogDir    string
	CloneDir     string

	ReviewRuns  string // one directory per review run
	BabysitRuns string // one directory per babysit run
	DepsCache   string // shared dependency trees, keyed by lock file hash

	Plist string

	// Where prbot kept the two files an upgrade starts from. Each is set only
	// while quorum has no file of its own, see Resolve.
	legacyConfig string
	legacyState  string
}

// ConfigSource is the file the configuration is read from: quorum's own, or
// prbot's while quorum has none. Saving always writes Config, after which this
// is Config as well.
func (p P) ConfigSource() string {
	if p.legacyConfig != "" && !exists(p.Config) {
		return p.legacyConfig
	}
	return p.Config
}

// LastPollFile is the heartbeat every finished poll writes.
func (p P) LastPollFile() string { return filepath.Join(p.StateDir, "last-poll") }

// CacheSizeFile is the last cache measurement, kept for the next process.
func (p P) CacheSizeFile() string { return filepath.Join(p.StateDir, "cache-size") }

// PollLock serialises polls.
func (p P) PollLock() string { return filepath.Join(p.StateDir, "poll.lock") }

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
// from scratch. Writes always go to the new location: the config is read
// through ConfigSource and saved to Config, and EnsureDirs copies the state
// file over before the first process that writes it does so.
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
		RulesDir:     filepath.Join(configHome, "quorum", "rules"),
		TestCmdDir:   filepath.Join(configHome, "quorum", "testcmd"),
		StateDir:     stateDir,
		StateFile:    filepath.Join(stateDir, "state.json"),
		PRStatesFile: filepath.Join(stateDir, "pr-states.json"),
		HistoryFile:  filepath.Join(stateDir, "history.jsonl"),
		Log:          filepath.Join(stateDir, "quorum.log"),
		RunningDir:   filepath.Join(stateDir, "running"),
		ManualDir:    filepath.Join(stateDir, "manual-reviews"),
		RunLogDir:    filepath.Join(stateDir, "runs"),
		CloneDir:     env("QUORUM_CLONE_DIR", filepath.Join(cacheRoot, "repos")),

		// The shared dependency cache is a sibling of the run directories, so
		// the run collector cannot mistake it for a run.
		ReviewRuns:  filepath.Join(cacheRoot, "reviews"),
		BabysitRuns: filepath.Join(cacheRoot, "babysit"),
		DepsCache:   filepath.Join(cacheRoot, "deps"),

		Plist: filepath.Join(home, "Library", "LaunchAgents", PlistLabel+".plist"),
	}

	// Fall back to prbot's locations while quorum has none of its own. Only
	// reads land there. The state file is the one path that names the old
	// location in the meantime, because a command that only reads state never
	// creates directories; EnsureDirs moves it on.
	if legacy := filepath.Join(configHome, "prbot", "config"); !exists(p.Config) && exists(legacy) {
		p.legacyConfig = legacy
	}
	if legacy := filepath.Join(stateHome, "prbot", "state.json"); !exists(p.StateFile) && exists(legacy) {
		p.legacyState, p.StateFile = p.StateFile, legacy
	}

	return p
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// RunRoots lists every directory run directories are created in. They share
// the dependency cache, so each has to be checked for live runs before a
// shared tree is collected.
func (p P) RunRoots() []string { return []string{p.ReviewRuns, p.BabysitRuns} }

// EnsureDirs creates everything a run needs. Callers that only read state can
// skip it; poll, review and babysit cannot.
//
// These are also the commands that write the state file, so this is where a
// state file still read from prbot's location is copied to quorum's own. The
// old file stays behind untouched.
func (p *P) EnsureDirs() error {
	dirs := append([]string{p.StateDir, p.RunningDir, p.ManualDir, p.RunLogDir, p.CloneDir},
		append(p.RunRoots(), p.DepsCache)...)
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if p.legacyState == "" {
		return nil
	}
	own, legacy := p.legacyState, p.StateFile
	if !exists(own) {
		data, err := os.ReadFile(legacy)
		if err != nil {
			return err
		}
		if err := atomicfile.Write(own, data); err != nil {
			return err
		}
	}
	p.StateFile, p.legacyState = own, ""
	return nil
}

// Package paths resolves every file prbot reads or writes.
//
// The layout matches what the shell version of prbot used, so an existing
// install keeps its config, its state and its logs across the upgrade.
package paths

import (
	"os"
	"path/filepath"
)

// P holds the resolved locations for one process.
type P struct {
	Config      string // the config file itself
	StateDir    string
	StateFile   string
	Log         string
	LockDir     string
	RunningDir  string
	RunLogDir   string
	CloneDir    string
	ReviewCache string // pr-codex-review's own cache, read to follow live runs
	Plist       string
}

// PlistLabel is the launchd job name. Changing it orphans installed agents.
const PlistLabel = "io.github.prbot"

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// Resolve honours the same environment variables the shell version did:
// PRBOT_CONFIG, PRBOT_STATE_DIR and PRBOT_CLONE_DIR win over the XDG defaults.
func Resolve() P {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.Getenv("HOME")
	}
	configHome := env("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	stateHome := env("XDG_STATE_HOME", filepath.Join(home, ".local", "state"))
	cacheHome := env("XDG_CACHE_HOME", filepath.Join(home, ".cache"))

	stateDir := env("PRBOT_STATE_DIR", filepath.Join(stateHome, "prbot"))

	return P{
		Config:      env("PRBOT_CONFIG", filepath.Join(configHome, "prbot", "config")),
		StateDir:    stateDir,
		StateFile:   filepath.Join(stateDir, "state.json"),
		Log:         filepath.Join(stateDir, "prbot.log"),
		LockDir:     filepath.Join(stateDir, "lock"),
		RunningDir:  filepath.Join(stateDir, "running"),
		RunLogDir:   filepath.Join(stateDir, "runs"),
		CloneDir:    env("PRBOT_CLONE_DIR", filepath.Join(cacheHome, "prbot", "repos")),
		ReviewCache: filepath.Join(cacheHome, "pr-codex-review"),
		Plist:       filepath.Join(home, "Library", "LaunchAgents", PlistLabel+".plist"),
	}
}

// EnsureDirs creates everything a run needs. Callers that only read state can
// skip it; poll and review cannot.
func (p P) EnsureDirs() error {
	for _, d := range []string{p.StateDir, p.RunningDir, p.RunLogDir, p.CloneDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}

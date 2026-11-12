package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// An upgrade from prbot reads its files until quorum has its own, and never
// writes them: the fallback used to replace the write path as well, so such an
// install kept saving to ~/.config/prbot/config.
func TestLegacyFilesAreReadButNeverWritten(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configHome, stateHome := filepath.Join(home, "cfg"), filepath.Join(home, "state")
	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_STATE_HOME", stateHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	for _, key := range []string{"QUORUM_CONFIG", "QUORUM_STATE_DIR", "QUORUM_CLONE_DIR"} {
		t.Setenv(key, "")
	}
	legacyConfig := filepath.Join(configHome, "prbot", "config")
	legacyState := filepath.Join(stateHome, "prbot", "state.json")
	write(t, legacyConfig, "REVIEWERS=3\n")
	write(t, legacyState, `{"schema":1,"prs":{}}`)

	p := Resolve()
	ownConfig := filepath.Join(configHome, "quorum", "config")
	ownState := filepath.Join(stateHome, "quorum", "state.json")
	if p.Config != ownConfig {
		t.Errorf("Config = %s, want the path saves go to, %s", p.Config, ownConfig)
	}
	if got := p.ConfigSource(); got != legacyConfig {
		t.Errorf("ConfigSource = %s, want the prbot file %s", got, legacyConfig)
	}
	if p.StateFile != legacyState {
		t.Errorf("StateFile = %s, want the prbot file until directories exist", p.StateFile)
	}

	// A save lands on Config, and from then on that is what is read.
	write(t, p.Config, "REVIEWERS=4\n")
	if got := p.ConfigSource(); got != ownConfig {
		t.Errorf("ConfigSource after a save = %s, want %s", got, ownConfig)
	}

	// The commands that write state create directories first.
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if p.StateFile != ownState {
		t.Errorf("StateFile after EnsureDirs = %s, want %s", p.StateFile, ownState)
	}
	if b, err := os.ReadFile(ownState); err != nil || string(b) != `{"schema":1,"prs":{}}` {
		t.Errorf("the state did not come along: %q, %v", b, err)
	}
	if _, err := os.Stat(legacyState); err != nil {
		t.Errorf("the prbot state file was touched: %v", err)
	}
	if again := Resolve(); again.StateFile != ownState || again.ConfigSource() != ownConfig {
		t.Errorf("the next process still uses prbot's files: %s, %s", again.StateFile, again.ConfigSource())
	}
}

func TestWithoutLegacyFilesEveryPathIsQuorumsOwn(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, key := range []string{"XDG_CONFIG_HOME", "XDG_STATE_HOME", "XDG_CACHE_HOME", "QUORUM_CONFIG", "QUORUM_STATE_DIR", "QUORUM_CLONE_DIR"} {
		t.Setenv(key, "")
	}
	p := Resolve()
	if p.ConfigSource() != p.Config || p.Config != filepath.Join(home, ".config", "quorum", "config") {
		t.Errorf("Config = %s, ConfigSource = %s", p.Config, p.ConfigSource())
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if p.StateFile != filepath.Join(home, ".local", "state", "quorum", "state.json") {
		t.Errorf("StateFile = %s", p.StateFile)
	}
}

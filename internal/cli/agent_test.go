package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/paths"
)

// A missing plist is the not-installed case, a freshly rendered one is
// current, and any drift in what install would write must read as stale.
func TestPlistStale(t *testing.T) {
	dir := t.TempDir()
	a := &app{
		cfg: config.Config{PollInterval: 300},
		p:   paths.P{Plist: filepath.Join(dir, "test.plist"), StateDir: dir},
	}

	if a.plistStale() {
		t.Fatal("a missing plist reads as stale, but it means the agent is not installed")
	}

	current, err := a.renderPlist()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.p.Plist, current, 0o644); err != nil {
		t.Fatal(err)
	}
	if a.plistStale() {
		t.Fatal("a freshly rendered plist reads as stale")
	}

	a.cfg.PollInterval = 120
	if !a.plistStale() {
		t.Fatal("a changed poll interval does not read as stale")
	}
}

// The interactive shell PATH must not change what the job bakes in. That was
// why doctor kept asking for quorum install after every make with a fine agent.
func TestRenderPlistIgnoresInteractivePATH(t *testing.T) {
	dir := t.TempDir()
	a := &app{
		cfg: config.Config{PollInterval: 300},
		p:   paths.P{Plist: filepath.Join(dir, "test.plist"), StateDir: dir},
	}

	t.Setenv("PATH", "/tmp/one-off-shell-bin:/usr/bin:/bin")
	first, err := a.renderPlist()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/tmp/another-shell-bin:/opt/weird:/usr/bin:/bin")
	second, err := a.renderPlist()
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("renderPlist changed when only the interactive PATH changed")
	}
	if strings.Contains(string(first), "/tmp/one-off-shell-bin") ||
		strings.Contains(string(first), "/tmp/another-shell-bin") ||
		strings.Contains(string(first), "/opt/weird") {
		t.Fatalf("plist baked interactive PATH entries:\n%s", first)
	}
	if !strings.Contains(string(first), agentPATH()) {
		t.Fatal("plist is missing the stable agent PATH")
	}
}

func TestWriteAgentPlistCreatesFile(t *testing.T) {
	dir := t.TempDir()
	a := &app{
		cfg: config.Config{PollInterval: 90},
		p:   paths.P{Plist: filepath.Join(dir, "LaunchAgents", "io.github.quorum.plist"), StateDir: dir},
	}
	if err := a.writeAgentPlist(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(a.p.Plist)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "<integer>90</integer>") {
		t.Fatalf("wrote interval wrong: %s", got)
	}
	if a.plistStale() {
		t.Fatal("just-written plist reads as stale")
	}
}

// A local build in ~/.local/bin has to win over the Homebrew installation, so
// the agent runs the binary the maintainer just built.
func TestAgentPATHPutsUserBinariesBeforeHomebrew(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	parts := strings.Split(agentPATH(), ":")
	local := slices.Index(parts, filepath.Join(home, ".local", "bin"))
	brew := slices.Index(parts, "/opt/homebrew/bin")
	if local < 0 || brew < 0 || local > brew {
		t.Fatalf("agentPATH = %q, want ~/.local/bin before /opt/homebrew/bin", agentPATH())
	}
	for _, part := range parts {
		if part == "" {
			t.Fatalf("empty PATH segment in %q", agentPATH())
		}
	}
}

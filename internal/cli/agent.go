package cli

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/yungweng/quorum/internal/paths"
)

// The launchd job. AbandonProcessGroup matters: reviews outlive the poll that
// started them, and without it launchd kills the whole group when poll exits.
// Deliberately neither ProcessType Background nor LowPriorityIO: both throttle
// disk I/O for the poll and everything it inherits to, which stretched the
// poll's cache walk from under a second to over a minute and made every poll
// outlast its own StartInterval. Reviews stay polite through NICE instead,
// which the runner sets on itself.
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array>
    <string>%s</string>
    <string>poll</string>
  </array>
  <key>StartInterval</key><integer>%d</integer>
  <key>RunAtLoad</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key><string>%s</string>
    <key>HOME</key><string>%s</string>
  </dict>
  <key>AbandonProcessGroup</key><true/>
</dict>
</plist>
`

func (a *app) cmdInstall(args []string) int {
	_ = args
	if runtime.GOOS != "darwin" {
		return a.die("install only supports macOS launchd; on Linux run `quorum poll` from a systemd timer or cron")
	}
	if _, err := a.findTools(); err != nil {
		return a.die("%v", err)
	}
	if err := a.p.EnsureDirs(); err != nil {
		return a.die("%v", err)
	}
	if _, err := os.Stat(a.p.Config); os.IsNotExist(err) {
		if err := a.cfg.Save(a.p.Config); err != nil {
			return a.die("could not write %s: %v", a.p.Config, err)
		}
		a.out.Printf("wrote a default config to %s\n", a.p.Config)
	}

	if err := os.MkdirAll(filepath.Dir(a.p.Plist), 0o755); err != nil {
		return a.die("%v", err)
	}

	// An installed prbot agent polls the same repositories with the same
	// GitHub account. Leaving it loaded would mean two agents racing for the
	// same review requests, so it goes before this one comes up.
	if legacy := legacyPlistPath(); legacy != "" {
		if _, err := os.Stat(legacy); err == nil {
			exec.Command("launchctl", "unload", legacy).Run()
			a.out.Printf("unloaded the old prbot agent at %s\n", legacy)
		}
	}

	if err := a.refreshAgent(); err != nil {
		return a.die("%v", err)
	}
	a.log.Printf("agent installed, polling every %ds", a.cfg.PollInterval)

	a.out.Printf("\n%s\n", a.out.Green("Agent installed."))
	a.out.Printf("  polls every %d seconds and reviews what asks for you\n", a.cfg.PollInterval)
	a.out.Printf("  config    %s\n", a.p.Config)
	a.out.Printf("  next      %s to change scope and limits\n", a.out.Bold("quorum setup"))
	a.out.Printf("            %s to see what it is doing\n\n", a.out.Bold("quorum"))
	return 0
}

func (a *app) cmdUninstall(args []string) int {
	_ = args
	exec.Command("launchctl", "unload", a.p.Plist).Run()
	if err := os.Remove(a.p.Plist); err != nil && !os.IsNotExist(err) {
		return a.die("%v", err)
	}
	a.log.Printf("agent removed")
	a.out.Printf("Agent removed. Config, state and cache are kept.\n")
	return 0
}

// refreshAgent writes the current launchd job and (re)loads it. It is the
// shared path for install, a poll that heals a stale job, doctor --fix, and
// a config save that changed the poll interval. It does not create a job that
// was never installed: callers that care check the plist exists first.
func (a *app) refreshAgent() error {
	if err := a.writeAgentPlist(); err != nil {
		return err
	}
	exec.Command("launchctl", "unload", a.p.Plist).Run()
	if out, err := exec.Command("launchctl", "load", a.p.Plist).CombinedOutput(); err != nil {
		return fmt.Errorf("launchctl load failed: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// writeAgentPlist renders and stores the job file without talking to launchd.
func (a *app) writeAgentPlist() error {
	if err := os.MkdirAll(filepath.Dir(a.p.Plist), 0o755); err != nil {
		return err
	}
	plist, err := a.renderPlist()
	if err != nil {
		return err
	}
	return os.WriteFile(a.p.Plist, plist, 0o644)
}

// renderPlist is the launchd job as install or a self-heal would write it.
//
// The binary path is deliberately not resolved through symlinks. Homebrew
// installs the binary into a versioned Cellar directory and links it into bin,
// so resolving would pin the agent to a path that the next upgrade deletes,
// leaving a job that launchd still loads and that can no longer run.
//
// PATH is a fixed agent path, not the interactive shell PATH: shell order and
// one-off dirs change every session, and baking them made every doctor run
// report a stale job after a brew or make that left the real job fine.
func (a *app) renderPlist() ([]byte, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	home := agentHome()
	return fmt.Appendf(nil, plistTemplate,
		paths.PlistLabel, self, a.cfg.PollInterval,
		filepath.Join(a.p.StateDir, "launchd.out.log"),
		filepath.Join(a.p.StateDir, "launchd.err.log"),
		xmlEscape(agentPATH()), xmlEscape(home)), nil
}

// agentHome is HOME for the launchd job. Prefer the resolved home so a shell
// with a temporary HOME does not rewrite the job to a different user.
func agentHome() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	return os.Getenv("HOME")
}

// agentPathDirs is the fixed list of directories the agent needs to find gh,
// git, codex, claude, grok and direnv outside a login shell. It is also what
// widenPath merges into the process PATH. Order is stable and free of
// interactive shell entries so two renders produce the same plist. npm's
// global prefix is not listed here: resolving it needs a subprocess and can
// differ between a terminal and launchd; widenPath still adds it to the
// process PATH when a tool is missing.
func agentPathDirs() []string {
	home, _ := os.UserHomeDir()
	return []string{
		home + "/.local/bin",
		home + "/bin",
		"/opt/homebrew/bin",
		"/usr/local/bin",
		home + "/.npm-global/bin",
		home + "/.bun/bin",
		home + "/.cargo/bin",
		"/usr/bin", "/bin", "/usr/sbin", "/sbin",
	}
}

// agentPATH is the PATH baked into the launchd job.
func agentPATH() string {
	return strings.Join(agentPathDirs(), ":")
}

// plistStale reports whether the installed job differs from what install
// would write now: another binary path, another poll interval, another agent
// PATH, or a template this version changed. The binary itself needs no
// reinstall to take effect when ProgramArguments already points at a stable
// path, because launchd starts each poll fresh through that path. A missing
// plist is not stale, that is the not-installed case.
func (a *app) plistStale() bool {
	installed, err := os.ReadFile(a.p.Plist)
	if err != nil {
		return false
	}
	want, err := a.renderPlist()
	if err != nil {
		return false
	}
	return !bytes.Equal(installed, want)
}

// agentLoaded reports whether launchd knows the job.
func (a *app) agentLoaded() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	return exec.Command("launchctl", "list", paths.PlistLabel).Run() == nil
}

// legacyPlistPath is where prbot installed its launchd job.
func legacyPlistPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "LaunchAgents", paths.LegacyPlistLabel+".plist")
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

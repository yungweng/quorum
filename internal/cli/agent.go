package cli

import (
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

	// Deliberately not resolved through symlinks. Homebrew installs the binary
	// into a versioned Cellar directory and links it into bin, so resolving
	// would pin the agent to a path that the next upgrade deletes, leaving a
	// job that launchd still loads and that can no longer run.
	self, err := os.Executable()
	if err != nil {
		return a.die("%v", err)
	}

	if err := os.MkdirAll(filepath.Dir(a.p.Plist), 0o755); err != nil {
		return a.die("%v", err)
	}
	// findTools widened PATH above. Bake it into the job so the agent can find
	// gh, git, codex and optional direnv outside a login shell.
	plist := fmt.Sprintf(plistTemplate,
		paths.PlistLabel, self, a.cfg.PollInterval,
		filepath.Join(a.p.StateDir, "launchd.out.log"),
		filepath.Join(a.p.StateDir, "launchd.err.log"),
		xmlEscape(os.Getenv("PATH")), os.Getenv("HOME"))
	if err := os.WriteFile(a.p.Plist, []byte(plist), 0o644); err != nil {
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

	exec.Command("launchctl", "unload", a.p.Plist).Run()
	if out, err := exec.Command("launchctl", "load", a.p.Plist).CombinedOutput(); err != nil {
		return a.die("launchctl load failed: %s", strings.TrimSpace(string(out)))
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

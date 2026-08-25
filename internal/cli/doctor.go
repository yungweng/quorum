package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/runner"
	"github.com/yungweng/quorum/internal/state"
	"github.com/yungweng/quorum/internal/ui"
)

// check is one line of the doctor report.
type check struct {
	name   string
	detail string
	level  int // 0 fine, 1 worth knowing, 2 broken
	fix    string
}

func (a *app) cmdDoctor(args []string) int {
	fix := false
	for _, arg := range args {
		if arg == "--fix" {
			fix = true
		}
	}

	checks := a.runChecks()
	worst := 0
	w := a.out
	fmt.Fprintln(w.Out)
	for _, c := range checks {
		mark := w.Green("ok  ")
		switch c.level {
		case 1:
			mark = w.Yellow("warn")
		case 2:
			mark = w.Red("fail")
		}
		worst = max(worst, c.level)
		w.Printf("  %s  %s %s\n", mark, w.Dim(ui.Pad(c.name, 16)), c.detail)
		if c.fix != "" {
			w.Printf("        %s %s\n", ui.Pad("", 16), w.Dim("→ "+c.fix))
		}
	}
	fmt.Fprintln(w.Out)

	if !fix {
		if worst > 0 {
			w.Printf("  %s\n\n", w.Dim("quorum doctor --fix applies what can be applied automatically"))
		}
		if worst == 2 {
			return 1
		}
		return 0
	}

	w.Printf("%s\n", w.Bold("applying fixes"))
	freed, removed, err := a.collect(false)
	if err != nil {
		return a.die("cache collection: %v", err)
	}
	if freed > 0 || removed > 0 {
		w.Printf("  cache      freed %s\n", ui.Bytes(freed))
	}
	if n := a.clearOrphans(); n > 0 {
		w.Printf("  state      cleared %d interrupted review(s)\n", n)
	}
	if runtime.GOOS == "darwin" {
		for _, dir := range []string{a.p.ReviewRuns, filepath.Dir(a.p.CloneDir)} {
			if isTimeMachineIncluded(dir) {
				if err := exec.Command("tmutil", "addexclusion", dir).Run(); err == nil {
					w.Printf("  backups    excluded %s from Time Machine\n", dir)
				}
			}
		}
		// Only refresh an existing job. Missing plist is uninstall or never
		// installed; that needs an explicit quorum install.
		if _, err := os.Stat(a.p.Plist); err == nil && a.plistStale() {
			if err := a.refreshAgent(); err != nil {
				w.Printf("  agent      could not refresh: %v\n", err)
			} else {
				w.Printf("  agent      refreshed the launchd job\n")
			}
		}
	}
	fmt.Fprintln(w.Out)
	return 0
}

func (a *app) runChecks() []check {
	var out []check
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	widenPath()

	// External tools.
	for _, spec := range []struct{ name, why string }{
		{"gh", "GitHub access"},
		{"git", "cloning and fetching"},

		{"codex", "the reviewer panel runs it"},
		{"claude", "the claude engine runs it"},
		{"grok", "the grok engine runs it"},
		{"direnv", "entering project environments"},
	} {
		path, err := exec.LookPath(spec.name)
		if err != nil {
			level, fix := 2, "install "+spec.name
			if spec.name == "direnv" {
				// Only projects with an .envrc need it.
				// REVIEW_ARGS is retired and only --dry-run is still read out of
				// it, so pointing at it here would be advice that does nothing.
				level, fix = 1, "install direnv, or pass --no-direnv"
			}
			if spec.name == "claude" {
				// Only needed when an engine setting selects it.
				level, fix = 1, "install Claude Code, or leave REVIEW_ENGINE/FIX_ENGINE at codex"
				if a.cfg.ReviewEngine == config.EngineClaude || a.cfg.FixEngine == config.EngineClaude {
					level, fix = 2, "install Claude Code; your config selects the claude engine"
				}
			}
			if spec.name == "grok" {
				level, fix = 1, "install the Grok CLI, or leave REVIEW_ENGINE/FIX_ENGINE at codex"
				if a.cfg.ReviewEngine == config.EngineGrok || a.cfg.FixEngine == config.EngineGrok {
					level, fix = 2, "install the Grok CLI; your config selects the grok engine"
				}
			}
			out = append(out, check{spec.name, "not found, needed for " + spec.why, level, fix})
			continue
		}
		out = append(out, check{spec.name, versionOf(path), 0, ""})
	}
	if runtime.GOOS == "darwin" {
		out = append(out, notificationChecks(a.cfg)...)
	}

	// GitHub authentication.
	if ghBin, err := exec.LookPath("gh"); err == nil {
		client := a.newGH(ghBin)
		client.Log = nil
		if login, err := client.Login(ctx); err != nil {
			out = append(out, check{"github auth", err.Error(), 2, "gh auth login"})
		} else {
			out = append(out, check{"github auth", "signed in as " + login, 0, ""})
		}
	}

	// The agent and whether it is actually working.
	switch {
	case runtime.GOOS != "darwin":
		out = append(out, check{"agent", "launchd is macOS only, run `quorum poll` from cron", 1, ""})
	case !a.agentLoaded():
		out = append(out, check{"agent", "not installed", 1, "quorum install"})
	default:
		at, open, ok := a.lastPoll()
		switch {
		case !ok:
			out = append(out, check{"agent", "loaded, but has not completed a poll yet", 1, "quorum poll"})
		case time.Since(at) > 3*time.Duration(a.cfg.PollInterval)*time.Second:
			out = append(out, check{"agent", "loaded, but the last poll was " + ui.Ago(at), 2,
				"quorum poll to see the error, then quorum install to reload the agent"})
		default:
			out = append(out, check{"agent", fmt.Sprintf("polled %s, %d open request(s)", ui.Ago(at), open), 0, ""})
		}
		if a.plistStale() {
			out = append(out, check{"agent job", "not what the current binary would write (binary path, interval or agent PATH changed)", 1,
				"healed on the next poll, or quorum doctor --fix / quorum install"})
		}
	}

	// Config.
	if a.configErr != nil {
		out = append(out, check{"config", a.configErr.Error(), 1, "quorum setup, or fix the file by hand"})
	} else {
		out = append(out, check{"config", a.p.Config, 0, ""})
	}

	// Recent GitHub trouble, straight from the log.
	if retries := a.countLog("retrying in"); retries > 0 {
		out = append(out, check{"network", fmt.Sprintf("%d GitHub call(s) had to be retried recently", retries), 1,
			"nothing to do, quorum retries by itself"})
	}

	// Interrupted reviews.
	if n := a.orphanCount(); n > 0 {
		out = append(out, check{"reviews", fmt.Sprintf("%d review(s) recorded as running with no process left", n), 1,
			"quorum doctor --fix"})
	}

	// Disk.
	size := a.cacheSize()
	limit := a.budgetBytes()
	switch {
	case limit <= 0:
		out = append(out, check{"cache", ui.Bytes(size) + ", no budget set", 0, ""})
	case size > limit:
		out = append(out, check{"cache", fmt.Sprintf("%s, over the %s budget", ui.Bytes(size), ui.Bytes(limit)), 1, "quorum gc"})
	default:
		out = append(out, check{"cache", fmt.Sprintf("%s of %s", ui.Bytes(size), ui.Bytes(limit)), 0, ""})
	}

	if runtime.GOOS == "darwin" && isTimeMachineIncluded(a.p.ReviewRuns) {
		out = append(out, check{"backups", "the review cache is being backed up by Time Machine", 1, "quorum doctor --fix"})
	}

	// Scheduling priority.
	if a.cfg.Nice <= 0 {
		out = append(out, check{"priority", "reviews run at normal priority", 1, "set NICE=10 in the config"})
	} else {
		out = append(out, check{"priority", fmt.Sprintf("reviews run at nice %d", a.cfg.Nice), 0, ""})
	}

	// How much can run at once. Codex reviewers spend their time waiting on the
	// network rather than on a core, so the ceiling worth checking is memory,
	// not the CPU count: roughly 150 MB per process from what runs have used.
	codex := a.cfg.Codex()
	budget := fmt.Sprintf("%d reviews x %d reviewers = up to %d Codex processes, about %s",
		a.cfg.MaxConcurrent, a.cfg.Reviewers, codex, ui.Bytes(int64(codex)*150*1024*1024))
	level, fixText := 0, ""
	if mem, ok := totalMemory(); ok && int64(codex)*150*1024*1024 > mem/2 {
		level = 1
		fixText = "lower MAX_CONCURRENT or REVIEWERS, this machine has " + ui.Bytes(mem) + " of memory"
	}
	out = append(out, check{"budget", budget, level, fixText})

	return out
}

func notificationChecks(cfg config.Config) []check {
	if !cfg.Notify {
		return nil
	}
	return []check{temporaryNotificationCheck()}
}

func temporaryNotificationCheck() check {
	path, err := exec.LookPath("terminal-notifier")
	if err != nil {
		return check{
			"notifications",
			"terminal-notifier not found; routine detached notifications are disabled",
			1,
			"brew install terminal-notifier",
		}
	}
	return check{"notifications", versionOf(path), 0, ""}
}

// versionOf asks a tool for its version, falling back to its path.
func versionOf(path string) string {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return path
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	if line == "" {
		return path
	}
	return line
}

// countLog counts how often a phrase appears in the recent log.
func (a *app) countLog(phrase string) int {
	n := 0
	for _, line := range a.log.Tail(500) {
		if strings.Contains(line, phrase) {
			n++
		}
	}
	return n
}

// orphanCount counts records that claim to be running with no live process.
func (a *app) orphanCount() int {
	file, err := state.Read(a.p.StateFile)
	if err != nil {
		return 0
	}
	live := map[string]bool{}
	for _, m := range runner.Live(a.p.RunningDir) {
		live[m.Key] = true
	}
	n := 0
	for key, rec := range file.PRs {
		if rec.Status == state.Running && !live[key] {
			n++
		}
	}
	return n
}

// isTimeMachineIncluded reports whether a directory would be backed up. The
// review cache is large, churning and reproducible, so it should not be.
func isTimeMachineIncluded(dir string) bool {
	if _, err := os.Stat(dir); err != nil {
		return false
	}
	out, err := exec.Command("tmutil", "isexcluded", dir).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "[Included]")
}

// totalMemory reports the machine's physical memory.
func totalMemory() (int64, bool) {
	out, err := exec.Command("sysctl", "-n", "hw.memsize").Output()
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

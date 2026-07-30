// Package cli is the quorum command line: argument parsing, the commands
// themselves, and the terminal reporting each one does.
//
// It exists as a package rather than as package main so that the binary's
// entry point stays a stub. Version lives in package main because the Homebrew
// formula sets it with -ldflags "-X main.Version=...", so it is passed in here
// rather than read from a constant.
package cli

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/logbook"
	"github.com/yungweng/quorum/internal/paths"
	"github.com/yungweng/quorum/internal/ui"
)

// Exit codes. The first five are inherited from babysit, so scripts wrapped
// around it keep working.
const (
	exitOK           = 0
	exitError        = 1
	exitGateAborted  = 2
	exitCIRed        = 3
	exitNotConverged = 4
	exitNoProgress   = 5
)

// app carries what every command needs.
type app struct {
	version string
	cfg     config.Config
	p       paths.P
	log     *logbook.Logger
	out     *ui.Writer
	err     *ui.Writer

	// configErr records a config file that could not be fully parsed. Commands
	// keep working on the defaults; doctor and status report it.
	configErr error

	// The last measurement of the cache, and when it was taken. Measuring walks
	// every cache file, so collection and its report must share the result.
	cacheBytes int64
	cacheAt    time.Time

	// The dashboard redraws frequently; keep it from spawning launchctl on
	// every frame while still noticing agent changes promptly.
	agentOn bool
	agentAt time.Time

	removeAll func(string) error // replaced by gc tests that need deterministic failures
}

// Run dispatches one command line and returns the process exit code. version
// is what package main was built with.
func Run(version string, args []string) int {
	return newApp(version).run(args)
}

func (a *app) run(args []string) int {
	if len(args) == 0 {
		a.usage()
		return exitOK
	}
	cmd := args[0]
	args = args[1:]

	switch cmd {
	case "status":
		return a.cmdStatus(args)
	case "review":
		return a.cmdReview(args)
	case "babysit":
		return a.cmdBabysit(args)
	case "watch":
		return a.cmdWatch(args)
	case "poll":
		return a.cmdPoll(args)
	case "run":
		return a.cmdRun(args)
	case "_review":
		return a.cmdReviewOne(args)
	case "logs":
		return a.cmdLogs(args)
	case "doctor":
		return a.cmdDoctor(args)
	case "setup":
		return a.cmdSetup(args)
	case "install":
		return a.cmdInstall(args)
	case "uninstall":
		return a.cmdUninstall(args)
	case "gc":
		return a.cmdGC(args)
	case "config":
		return a.cmdConfig(args)
	case "--version", "-v", "version":
		fmt.Println(a.version)
		return exitOK
	case "-h", "--help", "help":
		a.usage()
		return exitOK
	default:
		a.err.Printf("unknown command: %s\n\n", cmd)
		a.usage()
		return exitError
	}
}

func newApp(version string) *app {
	p := paths.Resolve()
	cfg, cfgErr := config.Load(p.Config)
	log := logbook.New(p.Log)
	out := ui.New(os.Stdout)
	// Only echo log lines to a terminal; under launchd they would just be
	// duplicated into the launchd log.
	if out.Color {
		log.Echo = func(s string) { fmt.Fprintln(os.Stdout, s) }
	}
	return &app{
		version:   version,
		cfg:       cfg,
		p:         p,
		log:       log,
		out:       out,
		err:       ui.New(os.Stderr),
		configErr: cfgErr,
	}
}

// tools resolves the external binaries quorum drives. launchd starts jobs with
// a minimal environment and never reads a shell profile, so PATH is widened
// with the usual install locations before anything is looked up.
//
// pr-codex-review is deliberately absent: the reviewer panel is part of this
// binary now, which also retires the version coupling the fix loop had to it.
type tools struct {
	GH    string
	Git   string
	Codex string
	// Direnv is optional: repositories without an .envrc never need it.
	Direnv string
}

func (a *app) findTools() (tools, error) {
	widenPath()
	var t tools
	var missing []string
	for _, spec := range []struct {
		name string
		dst  *string
	}{
		{"gh", &t.GH},
		{"git", &t.Git},
		{"codex", &t.Codex},
	} {
		path, err := exec.LookPath(spec.name)
		if err != nil {
			missing = append(missing, spec.name)
			continue
		}
		*spec.dst = path
	}
	t.Direnv, _ = exec.LookPath("direnv")

	if len(missing) > 0 {
		return t, fmt.Errorf("missing required tools: %s", strings.Join(missing, ", "))
	}
	return t, nil
}

func widenPath() {
	home, _ := os.UserHomeDir()
	extra := []string{
		home + "/.local/bin",
		home + "/bin",
		"/opt/homebrew/bin",
		"/usr/local/bin",
		home + "/.npm-global/bin",
		home + "/.bun/bin",
		home + "/.cargo/bin",
		"/usr/bin", "/bin", "/usr/sbin", "/sbin",
	}
	// npm puts globally installed tools into its own prefix, which is often
	// somewhere non-standard. Asking npm costs a subprocess, so skip it only
	// when the shell's existing PATH already resolves every supported tool.
	allResolved := true
	for _, name := range []string{"gh", "git", "codex", "direnv"} {
		if _, err := exec.LookPath(name); err != nil {
			allResolved = false
			break
		}
	}
	if !allResolved {
		if npm, err := exec.LookPath("npm"); err == nil {
			if out, err := exec.Command(npm, "prefix", "-g").Output(); err == nil {
				if prefix := strings.TrimSpace(string(out)); prefix != "" {
					extra = append([]string{prefix + "/bin"}, extra...)
				}
			}
		}
	}
	current := os.Getenv("PATH")
	have := map[string]bool{}
	for _, dir := range strings.Split(current, ":") {
		have[dir] = true
	}
	var add []string
	for _, dir := range extra {
		if !have[dir] {
			add = append(add, dir)
		}
	}
	if len(add) > 0 {
		os.Setenv("PATH", current+":"+strings.Join(add, ":"))
	}
}

func (a *app) newGH(bin string) *gh.Client {
	c := gh.New(bin)
	c.Log = func(format string, args ...any) { a.log.Printf(format, args...) }
	return c
}

func (a *app) newGit(bin string) git.G { return git.New(bin) }

func (a *app) usage() {
	a.out.Printf(`%s
A panel of Codex reviewers for your pull requests.

%s
  quorum <command> [options]

%s
  quorum watch           follow running, queued and finished work
  quorum review [pr]     review a PR and post the comment
  quorum babysit [pr]    review, fix and wait for CI until the PR is clean

  [pr] defaults to the pull request for the current branch.

%s
  quorum status          show one dashboard snapshot
  quorum run <pr>        hand one PR to the agent now
  quorum logs [-n N]     follow the log; use --no-follow to print and stop
  quorum poll            run one poll cycle now

%s
  quorum doctor [--fix]  check the setup and fix what it can
  quorum setup           configure scope, limits and notifications
  quorum install         install the launchd agent (polls every %ds)
  quorum uninstall       remove the agent but keep config and state
  quorum config          change settings; use --path for the file location
  quorum gc              trim the cache to its budget
  quorum version         print the version

Run quorum <command> --help for command options.

Requires: gh (authenticated), git and codex. direnv is optional.
Config: %s
`, a.out.Bold("quorum "+a.version), a.out.Bold("Usage:"),
		a.out.Bold("Main commands:"), a.out.Bold("Agent:"), a.out.Bold("Setup:"),
		a.cfg.PollInterval, a.p.Config)
}

// die prints to stderr and returns the exit code, so callers can `return a.die(...)`.
func (a *app) die(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "quorum: "+format+"\n", args...)
	return exitError
}

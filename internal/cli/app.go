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
	"github.com/yungweng/quorum/internal/engine/spec"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/history"
	"github.com/yungweng/quorum/internal/logbook"
	"github.com/yungweng/quorum/internal/paths"
	"github.com/yungweng/quorum/internal/runner"
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
	exitDiverged     = 6
	exitConflicts    = 7
	// exitUsageLimit is shared by review and babysit: the engine refused to
	// run because the account's usage limit is exhausted.
	exitUsageLimit = 8
)

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

	// tick advances once per watch frame and drives the one animation on the
	// dashboard. It only moves while work is in flight, so an idle dashboard
	// still renders an identical frame and is never repainted.
	tick int

	removeAll func(string) error // replaced by gc tests that need deterministic failures
}

// Runs started from a terminal are the reason the log exists: they never touch
// the state file, so without this they leave nothing behind and the dashboard
// can only ever show what the agent did.
func (a *app) logRun(run history.Run) {
	if run.Key == "" {
		return
	}
	if err := history.Append(a.p.HistoryFile, run); err != nil {
		a.log.Printf("could not add %s to the history log: %v", run.Key, err)
	}
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
	cfg, cfgErr := config.Load(p.ConfigSource())
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

func engineBinary(name string, bins spec.Bins, hint string) error {
	e := spec.Get(name)
	if bins.For(name) != "" {
		return nil
	}
	advice := "install " + e.Install
	if e.Name != spec.Codex {
		advice += " or switch back to " + spec.Codex
	}
	return fmt.Errorf("engine %s is selected (%s) but no %s binary was found on PATH; %s",
		e.Name, hint, e.Bin, advice)
}

// agentTools is findTools for the commands that run what the config says:
// poll, run and install. They need the engines the config selects, and only
// those; a setup that reviews and fixes with claude does not need codex.
func (a *app) agentTools() (runner.Tools, error) {
	t, err := a.findTools()
	if err != nil {
		return t, err
	}
	if err := engineBinary(a.cfg.ReviewEngine, t.Engines, "REVIEW_ENGINE"); err != nil {
		return t, err
	}
	if a.cfg.AgentAction == config.ActionBabysit {
		if err := engineBinary(a.cfg.FixEngine, t.Engines, "FIX_ENGINE"); err != nil {
			return t, err
		}
	}
	return t, nil
}

// findTools resolves the external binaries quorum drives. launchd starts jobs
// with a minimal environment and never reads a shell profile, so PATH is
// widened with the usual install locations before anything is looked up.
//
// gh and git are required. An engine is only needed when a setting or a flag
// selects it, which engineBinary checks at that point, and direnv only for
// repositories with an .envrc. Requiring codex here regardless kept a
// claude-only setup from running at all and the dashboard from learning merge
// states.
func (a *app) findTools() (runner.Tools, error) {
	widenPath()
	t := runner.Tools{Engines: spec.Bins{}}
	var missing []string
	for _, req := range []struct {
		name string
		dst  *string
	}{
		{"gh", &t.GH},
		{"git", &t.Git},
	} {
		path, err := exec.LookPath(req.name)
		if err != nil {
			missing = append(missing, req.name)
			continue
		}
		*req.dst = path
	}
	for _, e := range spec.All {
		if path, err := exec.LookPath(e.Bin); err == nil {
			t.Engines[e.Name] = path
		}
	}
	t.Direnv, _ = exec.LookPath("direnv")

	if len(missing) > 0 {
		return t, fmt.Errorf("missing required tools: %s", strings.Join(missing, ", "))
	}
	return t, nil
}

func (a *app) newRunner(t runner.Tools, client *gh.Client) *runner.Runner {
	return &runner.Runner{
		Cfg: a.cfg, P: a.p, Log: a.log,
		GH: client, Git: git.New(t.Git), Tools: t,
	}
}

func widenPath() {
	extra := agentPathDirs()
	// npm puts globally installed tools into its own prefix, which is often
	// somewhere non-standard. Asking npm costs a subprocess, so skip it only
	// when the shell's existing PATH already resolves every supported tool.
	// The optional tools stay in this list on purpose: npm's global prefix is
	// the standard install location for claude and a common one for direnv,
	// so a setup missing one pays the one subprocess rather than losing the
	// tool. The launchd job does not bake this prefix (see agentPathDirs);
	// only the live process path is widened.
	names := []string{"gh", "git"}
	for _, e := range spec.All {
		names = append(names, e.Bin)
	}
	allResolved := true
	for _, name := range append(names, "direnv") {
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

func (a *app) usage() {
	a.out.Printf(`%s
A panel of Codex reviewers for your pull requests.

%s
  quorum <command> [options]

%s
  quorum watch           follow running, queued and finished work
  quorum review [pr]     review a PR or the current pushed branch
  quorum babysit [pr]    review and fix a PR or the current pushed branch

  Without [pr], an open PR for the current branch is used when one exists;
  otherwise quorum works on the pushed branch without PR comments or PR CI.

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

Requires: gh (authenticated), git and the engine you review with (codex
unless REVIEW_ENGINE says otherwise). direnv is optional.
Config: %s
`, a.out.Bold("quorum "+a.version), a.out.Bold("Usage:"),
		a.out.Bold("Main commands:"), a.out.Bold("Agent:"), a.out.Bold("Setup:"),
		a.cfg.PollInterval, a.p.Config)
}

func (a *app) die(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "quorum: "+format+"\n", args...)
	return exitError
}

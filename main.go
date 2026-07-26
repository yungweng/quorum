// Command prbot watches GitHub for review requests aimed at you and runs
// pr-codex-review on each one.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/yungweng/prbot/internal/config"
	"github.com/yungweng/prbot/internal/gh"
	"github.com/yungweng/prbot/internal/logbook"
	"github.com/yungweng/prbot/internal/paths"
	"github.com/yungweng/prbot/internal/ui"
)

// Version is the release. The Homebrew formula builds it in with -ldflags.
var Version = "0.5.0"

// app carries what every command needs.
type app struct {
	cfg config.Config
	p   paths.P
	log *logbook.Logger
	out *ui.Writer
	err *ui.Writer

	// configErr records a config file that could not be fully parsed. Commands
	// keep working on the defaults; doctor and status report it.
	configErr error
}

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	a := newApp()

	cmd := ""
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}

	switch cmd {
	case "", "status":
		return a.cmdStatus(args)
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
		fmt.Println(Version)
		return 0
	case "-h", "--help", "help":
		a.usage()
		return 0
	default:
		a.err.Printf("unknown command: %s\n\n", cmd)
		a.usage()
		return 1
	}
}

func newApp() *app {
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
		cfg:       cfg,
		p:         p,
		log:       log,
		out:       out,
		err:       ui.New(os.Stderr),
		configErr: cfgErr,
	}
}

// tools resolves the external binaries prbot drives. launchd starts jobs with a
// minimal environment and never reads a shell profile, so PATH is widened with
// the usual install locations before anything is looked up.
type tools struct {
	GH     string
	Git    string
	Review string
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
		{"pr-codex-review", &t.Review},
	} {
		path, err := exec.LookPath(spec.name)
		if err != nil {
			missing = append(missing, spec.name)
			continue
		}
		*spec.dst = path
	}
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
	// npm puts globally installed tools such as codex into its own prefix,
	// which is often somewhere non-standard.
	if npm, err := exec.LookPath("npm"); err == nil {
		if out, err := exec.Command(npm, "prefix", "-g").Output(); err == nil {
			if prefix := strings.TrimSpace(string(out)); prefix != "" {
				extra = append([]string{prefix + "/bin"}, extra...)
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
	fmt.Printf(`prbot %s - run pr-codex-review automatically on PRs that request your review

  prbot                 what is running, queued and finished
  prbot watch           the same, redrawn as it changes
  prbot run <pr>        review one PR now: url, owner/repo#number, or number
  prbot logs [n]        follow the log
  prbot doctor [--fix]  check the setup and report what to do about it
  prbot setup           configure scope, limits and notifications
  prbot install         install the launchd agent (polls every %ds)
  prbot uninstall       remove the agent
  prbot poll            run one poll cycle by hand
  prbot gc              trim the review cache to its budget
  prbot config          change any setting, or --path for the file location

Requires: gh (authenticated), git, pr-codex-review.
Config: %s
`, Version, a.cfg.PollInterval, a.p.Config)
}

// die prints to stderr and returns the exit code, so callers can `return a.die(...)`.
func (a *app) die(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, "prbot: "+format+"\n", args...)
	return 1
}

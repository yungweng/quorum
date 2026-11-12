package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/config"
	macnotify "github.com/yungweng/quorum/internal/notify"
)

// A command without options used to ignore whatever followed its name, so
// asking uninstall for help removed the agent.
func TestHelpOnACommandWithoutOptionsDoesNotRunIt(t *testing.T) {
	a := testApp(t)
	var out bytes.Buffer
	a.out = plainWriter(&out)
	a.p.Plist = filepath.Join(t.TempDir(), "agent.plist")
	if err := os.WriteFile(a.p.Plist, []byte("plist"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, flag := range []string{"--help", "-h"} {
		out.Reset()
		if code := a.cmdUninstall([]string{flag}); code != exitOK {
			t.Fatalf("uninstall %s exited %d", flag, code)
		}
		if _, err := os.Stat(a.p.Plist); err != nil {
			t.Fatalf("uninstall %s removed the agent: %v", flag, err)
		}
		if !strings.Contains(out.String(), "usage: quorum uninstall") {
			t.Fatalf("uninstall %s printed %q", flag, out.String())
		}
	}
}

// A typo has to stop the command. `quorum gc --dryrun` deleted cache because
// the misspelt flag was simply not the one gc looked for.
func TestAMistypedOptionStopsTheCommand(t *testing.T) {
	a := testApp(t)
	a.out = plainWriter(&bytes.Buffer{})
	run := filepath.Join(a.p.ReviewRuns, "acme-api-pr-42-20260101-000000")
	if err := os.MkdirAll(run, 0o755); err != nil {
		t.Fatal(err)
	}
	// A budget of one byte would collect the directory if gc ran.
	a.cfg.CacheBudgetGB = 1e-9

	for _, args := range [][]string{{"--dryrun"}, {"--dry-run", "extra"}, {"--bogus=1"}} {
		if code := a.cmdGC(args); code != exitError {
			t.Errorf("gc %v exited %d, want %d", args, code, exitError)
		}
	}
	if _, err := os.Stat(run); err != nil {
		t.Fatalf("a rejected gc still collected: %v", err)
	}
	for name, cmd := range map[string]func([]string) int{
		"status": a.cmdStatus, "poll": a.cmdPoll, "install": a.cmdInstall,
		"uninstall": a.cmdUninstall, "setup": a.cmdSetup, "watch": a.cmdWatch,
		"doctor": a.cmdDoctor, "config": a.cmdConfig, "run": a.cmdRun,
	} {
		if code := cmd([]string{"--no-such-option"}); code != exitError {
			t.Errorf("%s --no-such-option exited %d, want %d", name, code, exitError)
		}
	}
}

// A switch is on by being there, so its value used to be ignored and
// --no-direnv=0 disabled direnv.
func TestASwitchReadsItsValue(t *testing.T) {
	flags := flagSet{"no-direnv": noValue, "effort": takesValue}
	for arg, want := range map[string]bool{
		"--no-direnv": true, "--no-direnv=1": true, "--no-direnv=true": true,
		"--no-direnv=0": false, "--no-direnv=false": false, "--no-direnv=off": false,
	} {
		parsed, err := parseArgs([]string{arg}, flags)
		if err != nil {
			t.Fatalf("%s: %v", arg, err)
		}
		if got := parsed.boolean("no-direnv"); got != want {
			t.Errorf("%s read as %v, want %v", arg, got, want)
		}
	}
	if _, err := parseArgs([]string{"--no-direnv=maybe"}, flags); err == nil {
		t.Error("--no-direnv=maybe was accepted")
	}
	// The last spelling wins, as it does for every other option.
	parsed, err := parseArgs([]string{"--no-direnv", "--no-direnv=0"}, flags)
	if err != nil || parsed.boolean("no-direnv") {
		t.Errorf("--no-direnv --no-direnv=0 left the switch on (err %v)", err)
	}
	// An option that takes a value keeps whatever follows the equals sign.
	if parsed, err := parseArgs([]string{"--effort=0"}, flags); err != nil || parsed.str("", "effort") != "0" {
		t.Errorf("--effort=0 lost its value (err %v)", err)
	}
}

// An action notice names quorum itself, a routine one does not; the terminal
// shows the prefix once either way.
func TestTerminalTitleCarriesThePrefixOnce(t *testing.T) {
	for in, want := range map[string]string{
		"Reviewed api#42": "quorum: Reviewed api#42",
		macnotify.ReadyToMerge("acme/api", 42).Title: "quorum: ready to merge",
	} {
		if got := terminalTitle(in); got != want {
			t.Errorf("terminalTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

// The wizard quoted the process counts of REVIEWERS=6 whatever was configured.
func TestSetupQuotesThePeakForTheConfiguredReviewers(t *testing.T) {
	cfg := config.Default()
	cfg.Reviewers = 2
	if got, want := peakLoad(cfg, 3), "up to 6 Codex processes, about 900.0 MB"; got != want {
		t.Errorf("peakLoad = %q, want %q", got, want)
	}
}

// QUORUM_CLONE_DIR can put the clones anywhere. What is kept out of backups is
// that directory, never its parent.
func TestBackupExclusionNamesTheCloneDirItself(t *testing.T) {
	a := testApp(t)
	root := filepath.Dir(a.p.ReviewRuns)
	a.p.CloneDir = filepath.Join(root, "repos")
	if got := a.cacheDirs(); len(got) != 1 || got[0] != root {
		t.Errorf("clones below the cache root: cacheDirs = %v, want only %s", got, root)
	}
	elsewhere := filepath.Join(t.TempDir(), "work", "quorum-clones")
	a.p.CloneDir = elsewhere
	got := a.cacheDirs()
	if len(got) != 2 || got[0] != root || got[1] != elsewhere {
		t.Errorf("cacheDirs = %v, want %s and %s", got, root, elsewhere)
	}
}

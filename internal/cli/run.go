package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/runner"
	"github.com/yungweng/quorum/internal/ui"
)

// cmdRun reviews one pull request now, whatever the queue and the filters say.
func (a *app) cmdRun(args []string) int {
	if len(args) == 0 {
		return a.die("usage: quorum run <pr-url> | <owner/repo#number> | <number>")
	}
	repo, number, err := a.resolvePR(args[0])
	if err != nil {
		return a.die("%v", err)
	}
	t, err := a.findTools()
	if err != nil {
		return a.die("%v", err)
	}
	if err := a.p.EnsureDirs(); err != nil {
		return a.die("%v", err)
	}

	key := fmt.Sprintf("%s#%d", repo, number)
	marker, got, err := runner.Acquire(a.p.RunningDir, key)
	if err != nil {
		return a.die("%v", err)
	}
	if !got {
		return a.die("a review of %s is already running", key)
	}
	defer marker.Release()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := a.newGH(t.GH)
	details, err := client.PRDetails(ctx, repo, number)
	if err != nil {
		return a.die("%v", err)
	}
	a.log.Echo = func(s string) { fmt.Println(s) }
	r := &runner.Runner{Cfg: a.cfg, P: a.p, Log: a.log, GitBin: t.Git, GHBin: t.GH,
		CodexBin: t.Codex, DirenvBin: t.Direnv, GH: client, Git: a.newGit(t.Git),
		TerminalNotify: func(title, body string) {
			a.out.Notify("quorum: "+title, body)
		},
	}
	if err := r.Review(ctx, key, repo, number, details.HeadRefOid, details.Title, details.Author.Login, "", runner.InvocationManual); err != nil {
		return 1
	}
	return 0
}

// parseLogArgs reads the arguments of `quorum logs`.
//
// -n takes a line count, the way every other tool spells it. It used to mean
// "do not follow", which is what --no-follow is for.
func parseLogArgs(args []string) (lines int, follow bool, err error) {
	lines, follow = 50, true
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; arg {
		case "--no-follow":
			follow = false
		case "-n", "--lines":
			if i+1 >= len(args) {
				return 0, false, fmt.Errorf("%s requires a number of lines", arg)
			}
			i++
			v, convErr := strconv.Atoi(args[i])
			if convErr != nil {
				return 0, false, fmt.Errorf("%s must be a number, got %q", arg, args[i])
			}
			lines = v
		default:
			v, convErr := strconv.Atoi(arg)
			if convErr != nil {
				return 0, false, fmt.Errorf("unknown argument: %s", arg)
			}
			lines = v
		}
		if lines < 0 {
			return 0, false, fmt.Errorf("number of lines must be non-negative, got %d", lines)
		}
	}
	return lines, follow, nil
}

// cmdLogs follows the log file, printing the tail first.
func (a *app) cmdLogs(args []string) int {
	n, follow, err := parseLogArgs(args)
	if err != nil {
		return a.die("%v", err)
	}
	for _, line := range a.log.Tail(n) {
		fmt.Println(line)
	}
	if !follow {
		return 0
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	f, err := os.Open(a.p.Log)
	if err != nil {
		if os.IsNotExist(err) {
			<-ctx.Done()
			return 0
		}
		return a.die("%v", err)
	}
	defer f.Close()
	f.Seek(0, io.SeekEnd)

	buf := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return 0
		default:
		}
		n, err := f.Read(buf)
		if n > 0 {
			os.Stdout.Write(buf[:n])
			continue
		}
		if err != nil && err != io.EOF {
			return a.die("%v", err)
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(300 * time.Millisecond):
		}
	}
}

// watchInterval is how often the dashboard is rebuilt.
//
// One frame costs about twelve microseconds and a handful of small file reads,
// so the interval is set by how quickly a change should show rather than by
// what it costs: a second is short enough that elapsed times count up visibly
// and a finished reviewer appears at once. It is not the agent's poll
// interval, which decides how often GitHub is asked and is configured
// separately. A pass that finds nothing changed paints nothing at all.
const watchInterval = time.Second

// cmdWatch redraws the dashboard until interrupted.
func (a *app) cmdWatch(args []string) int {
	_ = args
	if !a.out.Color {
		// Without a terminal there is nothing to redraw into.
		return a.cmdStatus(nil)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ends := newEndStates(a.p.PRStatesFile)
	if t, err := a.findTools(); err == nil {
		go a.trackEnds(ctx, t.GH, ends)
	}

	a.out.AltScreen(true)
	defer a.out.AltScreen(false)

	var frame bytes.Buffer
	painted := ""
	for {
		// Config and terminal size can change while watching.
		a.reload()
		// The frame is built in memory and reaches the terminal in one write.
		// Drawing straight to the screen shows it half finished on every pass.
		frame.Reset()
		screen := a.out.To(&frame)
		shown := a.dashboard(screen, ends.snapshot())
		screen.Printf("%s\n", screen.Dim("ctrl-c to leave"))
		ends.want(shown)

		// Most passes change nothing at all. Painting only what differs keeps
		// the terminal quiet instead of rewriting an identical screen.
		if next := frame.String(); next != painted {
			a.out.Paint(next)
			painted = next
		}
		a.tick++

		select {
		case <-ctx.Done():
			return 0
		case <-time.After(watchInterval):
		}
	}
}

// reload picks up config edits and a resized terminal between redraws.
func (a *app) reload() {
	cfg, err := config.Load(a.p.Config)
	a.cfg, a.configErr = cfg, err
	a.out = ui.New(os.Stdout)
}

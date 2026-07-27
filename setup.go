package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/yungweng/quorum/internal/config"
	"golang.org/x/term"
)

var errAborted = errors.New("aborted")

// option is one answer in the wizard.
type option struct {
	label  string
	detail string
	apply  func(*config.Config)
	// match reports whether this option describes the current configuration,
	// so the wizard opens on what is already set.
	match func(config.Config) bool
}

func (a *app) cmdSetup(args []string) int {
	_ = args
	if !term.IsTerminal(int(os.Stdin.Fd())) || !a.out.Color {
		return a.die("setup needs a terminal; edit %s instead", a.p.Config)
	}
	cfg := a.cfg
	w := a.out
	// One reader for the whole wizard. A fresh reader per question would throw
	// away bytes it had already buffered from the previous one.
	in := bufio.NewReader(os.Stdin)

	fmt.Fprintln(w.Out)
	w.Printf("%s\n", w.Bold("quorum setup"))
	w.Printf("%s\n", w.Dim("arrow keys to choose, enter to confirm, ctrl-c to leave"))

	questions := []struct {
		title   string
		options []option
	}{
		{
			"Which pull requests should quorum pick up?",
			[]option{
				{"every repository that asks you", "any org, any repo, including your personal ones",
					func(c *config.Config) { c.Orgs, c.Repos = nil, nil },
					func(c config.Config) bool { return len(c.Orgs) == 0 && len(c.Repos) == 0 }},
				{"only certain organisations", "you will be asked which",
					nil,
					func(c config.Config) bool { return len(c.Orgs) > 0 }},
				{"only certain repositories", "you will be asked which",
					nil,
					func(c config.Config) bool { return len(c.Repos) > 0 }},
			},
		},
		{
			"How many reviews should run at the same time?",
			[]option{
				{"3", "up to 18 Codex processes, about 2.6 GB",
					func(c *config.Config) { c.MaxConcurrent = 3 },
					func(c config.Config) bool { return c.MaxConcurrent <= 3 }},
				{"6", "up to 36 Codex processes, about 5.3 GB",
					func(c *config.Config) { c.MaxConcurrent = 6 },
					func(c config.Config) bool { return c.MaxConcurrent > 3 && c.MaxConcurrent <= 6 }},
				{"10", "up to 60 Codex processes, about 8.8 GB",
					func(c *config.Config) { c.MaxConcurrent = 10 },
					func(c config.Config) bool { return c.MaxConcurrent > 6 }},
			},
		},
		{
			"How often should quorum look for new requests?",
			[]option{
				{"every 2 minutes", "picks things up quickly", func(c *config.Config) { c.PollInterval = 120 },
					func(c config.Config) bool { return c.PollInterval <= 120 }},
				{"every 5 minutes", "the default", func(c *config.Config) { c.PollInterval = 300 },
					func(c config.Config) bool { return c.PollInterval > 120 && c.PollInterval <= 300 }},
				{"every 15 minutes", "quietest", func(c *config.Config) { c.PollInterval = 900 },
					func(c config.Config) bool { return c.PollInterval > 300 }},
			},
		},
		{
			"What about your own pull requests?",
			[]option{
				{"skip them", "review one on purpose with: quorum run <pr>",
					func(c *config.Config) { c.SkipOwn = true },
					func(c config.Config) bool { return c.SkipOwn }},
				{"review them too", "whenever a review is requested from you on your own PR",
					func(c *config.Config) { c.SkipOwn = false },
					func(c config.Config) bool { return !c.SkipOwn }},
			},
		},
		{
			"Notifications?",
			[]option{
				{"when a review finishes or fails", "click to open the posted comment",
					func(c *config.Config) { c.Notify = true },
					func(c config.Config) bool { return c.Notify }},
				{"none", "check with: quorum",
					func(c *config.Config) { c.Notify = false },
					func(c config.Config) bool { return !c.Notify }},
			},
		},
		{
			"Should reviews be posted to GitHub?",
			[]option{
				{"post them", "a comment on the pull request",
					func(c *config.Config) { c.Post = true },
					func(c config.Config) bool { return c.Post }},
				{"keep them local", "findings stay in the run directory",
					func(c *config.Config) { c.Post = false },
					func(c config.Config) bool { return !c.Post }},
			},
		},
		{
			"What should the agent do with a PR that asks for your review?",
			[]option{
				{"review it", "post one review and stop",
					func(c *config.Config) { c.AgentAction = config.ActionReview },
					func(c config.Config) bool { return c.AgentAction != config.ActionBabysit }},
				{"babysit it", "review, fix the findings, wait for CI, repeat until clean",
					func(c *config.Config) { c.AgentAction = config.ActionBabysit },
					func(c config.Config) bool { return c.AgentAction == config.ActionBabysit }},
			},
		},
	}

	for i, q := range questions {
		choice, err := a.ask(in, fmt.Sprintf("%d/%d  %s", i+1, len(questions), q.title), q.options, cfg)
		if err != nil {
			w.Printf("\n%s\n", w.Dim("nothing changed"))
			return 1
		}
		opt := q.options[choice]
		if opt.apply != nil {
			opt.apply(&cfg)
			continue
		}
		// The scope answers need a list typed in.
		switch choice {
		case 1:
			list, err := a.askText(in, "organisations, space separated", strings.Join(cfg.Orgs, " "))
			if err != nil {
				return 1
			}
			cfg.Orgs, cfg.Repos = strings.Fields(list), nil
		case 2:
			list, err := a.askText(in, "repositories as owner/name, space separated", strings.Join(cfg.Repos, " "))
			if err != nil {
				return 1
			}
			cfg.Repos, cfg.Orgs = strings.Fields(list), nil
		}
	}

	if err := cfg.Save(a.p.Config); err != nil {
		return a.die("could not write %s: %v", a.p.Config, err)
	}
	fmt.Fprintln(w.Out)
	w.Printf("%s %s\n", w.Green("saved"), a.p.Config)

	// The poll interval lives in the launchd job, so it only takes effect after
	// the agent is written again.
	if runtime.GOOS == "darwin" && a.agentLoaded() && cfg.PollInterval != a.cfg.PollInterval {
		a.cfg = cfg
		if code := a.cmdInstall(nil); code != 0 {
			return code
		}
		return 0
	}
	a.cfg = cfg
	if !a.agentLoaded() && runtime.GOOS == "darwin" {
		w.Printf("%s %s\n", w.Dim("next"), w.Bold("quorum install")+w.Dim(" to start the agent"))
	}
	fmt.Fprintln(w.Out)
	return 0
}

// ask draws a list and returns the chosen index.
func (a *app) ask(in *bufio.Reader, title string, opts []option, cfg config.Config) (int, error) {
	cursor := 0
	for i, o := range opts {
		if o.match != nil && o.match(cfg) {
			cursor = i
			break
		}
	}

	fd := int(os.Stdin.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return 0, err
	}
	defer term.Restore(fd, old)

	w := a.out
	fmt.Fprint(w.Out, "\r\n"+w.Bold(title)+"\r\n")

	draw := func(first bool) {
		if !first {
			// Move back over the options to redraw them in place.
			fmt.Fprintf(w.Out, "\x1b[%dA", len(opts))
		}
		for i, o := range opts {
			pointer, label := "  ", o.label
			if i == cursor {
				pointer, label = w.Cyan("› "), w.Bold(o.label)
			}
			fmt.Fprintf(w.Out, "\r\x1b[K%s%s  %s\r\n", pointer, label, w.Dim(o.detail))
		}
	}
	draw(true)

	// Read one byte at a time. Keys can arrive several at once, whether typed
	// quickly or piped in, and a reader that takes a fixed sized chunk would
	// silently drop everything after the first key in the chunk.
	for {
		b, err := in.ReadByte()
		if err != nil {
			return 0, errAborted
		}
		switch b {
		case '\r', '\n':
			return cursor, nil
		case 3, 4, 'q': // ctrl-c, ctrl-d
			return 0, errAborted
		case 'k':
			cursor = max(cursor-1, 0)
		case 'j':
			cursor = min(cursor+1, len(opts)-1)
		case 27: // an escape sequence: ESC [ A/B for the arrow keys
			if next, err := in.ReadByte(); err != nil || next != '[' {
				continue
			}
			code, err := in.ReadByte()
			if err != nil {
				return 0, errAborted
			}
			switch code {
			case 'A':
				cursor = max(cursor-1, 0)
			case 'B':
				cursor = min(cursor+1, len(opts)-1)
			default:
				continue
			}
		default:
			continue
		}
		draw(false)
	}
}

// askText reads one line, showing what is currently set.
func (a *app) askText(in *bufio.Reader, prompt, current string) (string, error) {
	w := a.out
	fmt.Fprintln(w.Out)
	if current != "" {
		w.Printf("%s %s\n", w.Dim("currently"), current)
	}
	w.Printf("%s ", w.Bold(prompt+":"))
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", errAborted
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return current, nil
	}
	return line, nil
}

package cli

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/codex"
	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/ui"
	"golang.org/x/term"
)

// setting is one editable row of `quorum config`. Everything quorum reads from
// the config file appears here: a value you can only reach by opening the file
// in an editor is a value you forget you set.
type setting struct {
	name  string
	value func(config.Config) string
	edit  func(*app, *bufio.Reader, *config.Config) error
}

func (a *app) settings() []setting {
	return []setting{
		{"scope", func(c config.Config) string {
			switch {
			case len(c.Repos) > 0:
				return strings.Join(c.Repos, " ")
			case len(c.Orgs) > 0:
				return strings.Join(c.Orgs, " ")
			}
			return "every repository that asks you"
		}, (*app).editScope},

		{"never review", func(c config.Config) string {
			return orNone(strings.Join(c.ExcludeRepos, " "))
		}, func(a *app, in *bufio.Reader, c *config.Config) error {
			v, err := a.askText(in, "repositories to never review, as owner/name", strings.Join(c.ExcludeRepos, " "))
			if err != nil {
				return err
			}
			c.ExcludeRepos = fieldsOrNil(v)
			return nil
		}},

		{"team requests", func(c config.Config) string {
			if !c.IncludeTeams {
				return "ignored, only requests aimed at you directly"
			}
			if len(c.Teams) > 0 {
				return strings.Join(c.Teams, " ")
			}
			return "picked up, teams discovered automatically"
		}, func(a *app, in *bufio.Reader, c *config.Config) error {
			return a.pick(in, "Requests aimed at a team you belong to?", c, []option{
				{"pick them up", "your teams are discovered automatically",
					func(c *config.Config) { c.IncludeTeams, c.Teams = true, nil },
					func(c config.Config) bool { return c.IncludeTeams && len(c.Teams) == 0 }},
				{"pick them up, but only for teams I name", "you will be asked which", nil,
					func(c config.Config) bool { return c.IncludeTeams && len(c.Teams) > 0 }},
				{"ignore them", "only requests aimed at you personally",
					func(c *config.Config) { c.IncludeTeams = false },
					func(c config.Config) bool { return !c.IncludeTeams }},
			}, func(choice int) error {
				if choice != 1 {
					return nil
				}
				v, err := a.askText(in, "teams as org/slug", strings.Join(c.Teams, " "))
				if err != nil {
					return err
				}
				c.IncludeTeams, c.Teams = true, fieldsOrNil(v)
				return nil
			})
		}},

		{"reviews at once", func(c config.Config) string {
			return fmt.Sprintf("%d", c.MaxConcurrent)
		}, func(a *app, in *bufio.Reader, c *config.Config) error {
			return a.editNumber(in, "how many reviews may run at the same time", c.MaxConcurrent, 1, 20, &c.MaxConcurrent)
		}},

		{"reviewers each", func(c config.Config) string {
			return fmt.Sprintf("%d, all in parallel, so up to %d Codex processes at peak",
				c.Reviewers, c.Codex())
		}, func(a *app, in *bufio.Reader, c *config.Config) error {
			return a.editNumber(in, "how many reviewer passes per review", c.Reviewers, 1, 12, &c.Reviewers)
		}},

		{"poll interval", func(c config.Config) string {
			return fmt.Sprintf("every %s", ui.Duration(secs(c.PollInterval)))
		}, func(a *app, in *bufio.Reader, c *config.Config) error {
			return a.pick(in, "How often should quorum look for new requests?", c, []option{
				{"every 2 minutes", "picks things up quickly",
					func(c *config.Config) { c.PollInterval = 120 },
					func(c config.Config) bool { return c.PollInterval <= 120 }},
				{"every 5 minutes", "the default",
					func(c *config.Config) { c.PollInterval = 300 },
					func(c config.Config) bool { return c.PollInterval > 120 && c.PollInterval <= 300 }},
				{"every 15 minutes", "quietest",
					func(c *config.Config) { c.PollInterval = 900 },
					func(c config.Config) bool { return c.PollInterval > 300 }},
			}, nil)
		}},

		{"attempts", func(c config.Config) string {
			return fmt.Sprintf("%d before quorum gives up on a request", c.MaxRetries)
		}, func(a *app, in *bufio.Reader, c *config.Config) error {
			return a.editNumber(in, "attempts per request before giving up", c.MaxRetries, 1, 10, &c.MaxRetries)
		}},

		{"priority", func(c config.Config) string {
			if c.Nice <= 0 {
				return "normal, reviews compete with your own work"
			}
			return fmt.Sprintf("nice %d, reviews give way to your own work", c.Nice)
		}, func(a *app, in *bufio.Reader, c *config.Config) error {
			return a.pick(in, "What priority should reviews run at?", c, []option{
				{"below your own work", "nice 10",
					func(c *config.Config) { c.Nice = 10 },
					func(c config.Config) bool { return c.Nice > 0 }},
				{"normal", "reviews compete with everything else",
					func(c *config.Config) { c.Nice = 0 },
					func(c config.Config) bool { return c.Nice <= 0 }},
			}, nil)
		}},

		{"load limit", func(c config.Config) string {
			if c.LoadLimit <= 0 {
				return "off, reviews start whenever they are requested"
			}
			return fmt.Sprintf("hold new reviews back above a 1-minute load of %.0f", c.LoadLimit)
		}, func(a *app, in *bufio.Reader, c *config.Config) error {
			return a.pick(in, "Hold reviews back while the machine is busy?", c, []option{
				{"no", "a requested review starts, whatever else is going on",
					func(c *config.Config) { c.LoadLimit = 0 },
					func(c config.Config) bool { return c.LoadLimit <= 0 }},
				{"yes, above a load I name", "they wait in the queue and start on a later poll", nil,
					func(c config.Config) bool { return c.LoadLimit > 0 }},
			}, func(choice int) error {
				if choice != 1 {
					return nil
				}
				v, err := a.askText(in, "hold back above this 1-minute load", trimNum(c.LoadLimit))
				if err != nil {
					return err
				}
				if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f > 0 {
					c.LoadLimit = f
				}
				return nil
			})
		}},

		{"cache budget", func(c config.Config) string {
			if c.CacheBudgetGB <= 0 {
				return "off, the cache is never trimmed"
			}
			return fmt.Sprintf("%s GB across runs and dependency trees", trimNum(c.CacheBudgetGB))
		}, func(a *app, in *bufio.Reader, c *config.Config) error {
			v, err := a.askText(in, "cache budget in GB, 0 turns it off", trimNum(c.CacheBudgetGB))
			if err != nil {
				return err
			}
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && f >= 0 {
				c.CacheBudgetGB = f
			}
			return nil
		}},

		{"drafts", skipLabel(func(c config.Config) bool { return c.SkipDrafts }),
			skipEdit("Review draft pull requests?", func(c *config.Config, v bool) { c.SkipDrafts = v },
				func(c config.Config) bool { return c.SkipDrafts })},

		{"fork PRs", skipLabel(func(c config.Config) bool { return c.SkipForks }),
			skipEdit("Review pull requests from forks? They run their own code on this machine.",
				func(c *config.Config, v bool) { c.SkipForks = v },
				func(c config.Config) bool { return c.SkipForks })},

		{"bot PRs", skipLabel(func(c config.Config) bool { return c.SkipBots }),
			skipEdit("Review pull requests opened by a bot?",
				func(c *config.Config, v bool) { c.SkipBots = v },
				func(c config.Config) bool { return c.SkipBots })},

		{"your own PRs", skipLabel(func(c config.Config) bool { return c.SkipOwn }),
			skipEdit("Review your own pull requests?",
				func(c *config.Config, v bool) { c.SkipOwn = v },
				func(c config.Config) bool { return c.SkipOwn })},

		{"notifications", func(c config.Config) string {
			if c.Notify {
				return "when a review finishes or fails"
			}
			return "off"
		}, func(a *app, in *bufio.Reader, c *config.Config) error {
			return a.pick(in, "Notifications?", c, []option{
				{"when a review finishes or fails", "click to open the posted comment",
					func(c *config.Config) { c.Notify = true },
					func(c config.Config) bool { return c.Notify }},
				{"none", "check with: quorum",
					func(c *config.Config) { c.Notify = false },
					func(c config.Config) bool { return !c.Notify }},
			}, nil)
		}},

		{"posting", func(c config.Config) string {
			if !c.Post {
				return "kept local, nothing is posted to GitHub"
			}
			return "posted as a comment on the pull request"
		}, func(a *app, in *bufio.Reader, c *config.Config) error {
			return a.pick(in, "Should reviews be posted to GitHub?", c, []option{
				{"post them", "a comment on the pull request",
					func(c *config.Config) { c.Post = true },
					func(c config.Config) bool { return c.Post }},
				{"keep them local", "findings stay in the run directory",
					func(c *config.Config) { c.Post = false },
					func(c config.Config) bool { return !c.Post }},
			}, nil)
		}},

		{"review model", func(c config.Config) string {
			return fmt.Sprintf("%s, effort %s", c.ReviewModel, c.ReviewEffort)
		}, func(a *app, in *bufio.Reader, c *config.Config) error {
			v, err := a.askText(in, "model for the reviewers and the aggregator", c.ReviewModel)
			if err != nil {
				return err
			}
			if v = strings.TrimSpace(v); v != "" {
				c.ReviewModel = v
			}
			return a.pick(in, "How hard should the reviewers think?", c, effortOptions(
				func(c *config.Config) *string { return &c.ReviewEffort }), nil)
		}},

		{"agent does", func(c config.Config) string {
			if c.AgentAction == config.ActionBabysit {
				return "the full fix loop: review, fix, CI, repeat"
			}
			return "one review, posted as a comment"
		}, func(a *app, in *bufio.Reader, c *config.Config) error {
			return a.pick(in, "What should the agent do with a PR that asks for your review?", c, []option{
				{"review it", "post one review and stop",
					func(c *config.Config) { c.AgentAction = config.ActionReview },
					func(c config.Config) bool { return c.AgentAction != config.ActionBabysit }},
				{"babysit it", "review, fix the findings, wait for CI, repeat until clean",
					func(c *config.Config) { c.AgentAction = config.ActionBabysit },
					func(c config.Config) bool { return c.AgentAction == config.ActionBabysit }},
			}, nil)
		}},

		autoMergeSetting("agent auto-merge", "Auto-merge after an agent run?",
			func(c config.Config) bool { return c.AutoMergeAgent },
			func(c *config.Config, v bool) { c.AutoMergeAgent = v }),

		autoMergeSetting("review auto-merge", "Auto-merge after a manual quorum review?",
			func(c config.Config) bool { return c.AutoMergeReview },
			func(c *config.Config, v bool) { c.AutoMergeReview = v }),

		autoMergeSetting("babysit auto-merge", "Auto-merge after a manual quorum babysit?",
			func(c config.Config) bool { return c.AutoMergeBabysit },
			func(c *config.Config, v bool) { c.AutoMergeBabysit = v }),

		{"fix sessions", func(c config.Config) string {
			model := c.FixModel
			if model == "" {
				model = "your codex default"
			}
			return fmt.Sprintf("%s, up to %d rounds, %s per step",
				model, c.MaxIter, config.FormatDuration(c.FixTimeout))
		}, func(a *app, in *bufio.Reader, c *config.Config) error {
			v, err := a.askText(in, "model for the fix sessions (empty keeps your codex default)", c.FixModel)
			if err != nil {
				return err
			}
			c.FixModel = strings.TrimSpace(v)
			n, err := a.askText(in, "maximum review to fix rounds", fmt.Sprint(c.MaxIter))
			if err != nil {
				return err
			}
			if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil && i >= 1 {
				c.MaxIter = i
			}
			return nil
		}},
	}
}

func autoMergeSetting(name, title string, enabled func(config.Config) bool, set func(*config.Config, bool)) setting {
	return setting{name, func(c config.Config) string {
		if enabled(c) {
			return "approve clean PRs and ask GitHub to merge them"
		}
		return "off"
	}, func(a *app, in *bufio.Reader, c *config.Config) error {
		return a.pick(in, title, c, []option{
			{"off", "leave the pull request open", func(c *config.Config) { set(c, false) },
				func(c config.Config) bool { return !enabled(c) }},
			{"on", "approve and merge only the exact reviewed head", func(c *config.Config) { set(c, true) },
				func(c config.Config) bool { return enabled(c) }},
		}, nil)
	}}
}

// cmdConfig shows every setting with its current value and lets one be changed
// at a time. --path prints the file location instead, for scripts.
func (a *app) cmdConfig(args []string) int {
	for _, arg := range args {
		if arg == "--path" || arg == "-p" {
			if _, err := os.Stat(a.p.Config); os.IsNotExist(err) {
				if err := a.cfg.Save(a.p.Config); err != nil {
					return a.die("%v", err)
				}
			}
			fmt.Println(a.p.Config)
			return 0
		}
	}
	if !term.IsTerminal(int(os.Stdin.Fd())) || !a.out.Color {
		// Not a terminal: print the settings instead of trying to draw a menu.
		a.printSettings()
		return 0
	}
	return a.settingsMenu()
}

func (a *app) printSettings() {
	w := a.out
	w.Printf("%s\n", a.p.Config)
	for _, s := range a.settings() {
		w.Printf("  %s %s\n", ui.Pad(s.name, 16), s.value(a.cfg))
	}
}

// settingsMenu is the interactive editor. It keeps a working copy so leaving
// without saving really changes nothing.
func (a *app) settingsMenu() int {
	w := a.out
	in := bufio.NewReader(os.Stdin)
	cfg := a.cfg
	items := a.settings()
	cursor, dirty := 0, false

	draw := func(first bool) {
		if !first {
			fmt.Fprintf(w.Out, "\x1b[%dA", len(items)+2)
		}
		for i, s := range items {
			pointer, name := "  ", ui.Pad(s.name, 16)
			if i == cursor {
				pointer, name = w.Cyan("› "), w.Bold(ui.Pad(s.name, 16))
			}
			fmt.Fprintf(w.Out, "\r\x1b[K%s%s %s\r\n", pointer, name, w.Dim(s.value(cfg)))
		}
		state := a.p.Config
		if dirty {
			state = w.Yellow("unsaved changes") + w.Dim("  "+a.p.Config)
		} else {
			state = w.Dim(state)
		}
		fmt.Fprintf(w.Out, "\r\x1b[K\r\n\r\x1b[K%s\r\n", state)
	}

	fmt.Fprintln(w.Out)
	w.Printf("%s\n", w.Bold("quorum config"))
	w.Printf("%s\n\n", w.Dim("↑↓ to move, enter to change, s to save, q to leave"))
	draw(true)

	for {
		fd := int(os.Stdin.Fd())
		old, err := term.MakeRaw(fd)
		if err != nil {
			return a.die("%v", err)
		}
		key, err := readKey(in)
		term.Restore(fd, old)
		if err != nil {
			return 1
		}

		switch key {
		case keyUp:
			cursor = max(cursor-1, 0)
		case keyDown:
			cursor = min(cursor+1, len(items)-1)
		case keyEnter:
			before := items[cursor].value(cfg)
			// Redrawing from scratch after the sub-prompt is simpler than
			// trying to work out how many lines it printed.
			fmt.Fprintln(w.Out)
			if err := items[cursor].edit(a, in, &cfg); err != nil && err != errAborted {
				return a.die("%v", err)
			}
			if items[cursor].value(cfg) != before {
				dirty = true
			}
			w.Home()
			fmt.Fprintln(w.Out)
			w.Printf("%s\n", w.Bold("quorum config"))
			w.Printf("%s\n\n", w.Dim("↑↓ to move, enter to change, s to save, q to leave"))
			draw(true)
			continue
		case keySave:
			if err := cfg.Save(a.p.Config); err != nil {
				return a.die("could not write %s: %v", a.p.Config, err)
			}
			restart := cfg.PollInterval != a.cfg.PollInterval
			a.cfg = cfg
			fmt.Fprintln(w.Out)
			w.Printf("%s %s\n", w.Green("saved"), a.p.Config)
			// The poll interval lives in the launchd job, not in the config, so
			// it only takes effect once the agent is written again.
			if restart && a.agentLoaded() {
				return a.cmdInstall(nil)
			}
			fmt.Fprintln(w.Out)
			return 0
		case keyQuit:
			fmt.Fprintln(w.Out)
			if dirty {
				w.Printf("%s\n\n", w.Dim("left without saving, nothing changed"))
			}
			return 0
		default:
			continue
		}
		draw(false)
	}
}

// key codes the menu understands.
type keyCode int

const (
	keyNone keyCode = iota
	keyUp
	keyDown
	keyEnter
	keySave
	keyQuit
)

func readKey(in *bufio.Reader) (keyCode, error) {
	b, err := in.ReadByte()
	if err != nil {
		return keyNone, err
	}
	switch b {
	case '\r', '\n':
		return keyEnter, nil
	case 's':
		return keySave, nil
	case 'q', 3, 4:
		return keyQuit, nil
	case 'k':
		return keyUp, nil
	case 'j':
		return keyDown, nil
	case 27:
		if next, err := in.ReadByte(); err != nil || next != '[' {
			return keyQuit, nil
		}
		code, err := in.ReadByte()
		if err != nil {
			return keyNone, err
		}
		switch code {
		case 'A':
			return keyUp, nil
		case 'B':
			return keyDown, nil
		}
	}
	return keyNone, nil
}

// pick runs a list prompt against the working copy. after is called with the
// chosen index for answers that need a follow-up question.
func (a *app) pick(in *bufio.Reader, title string, cfg *config.Config, opts []option, after func(int) error) error {
	choice, err := a.ask(in, title, opts, *cfg)
	if err != nil {
		return err
	}
	if opts[choice].apply != nil {
		opts[choice].apply(cfg)
	}
	if after != nil {
		return after(choice)
	}
	return nil
}

func (a *app) editNumber(in *bufio.Reader, prompt string, current, lo, hi int, dst *int) error {
	v, err := a.askText(in, fmt.Sprintf("%s (%d to %d)", prompt, lo, hi), strconv.Itoa(current))
	if err != nil {
		return err
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < lo || n > hi {
		a.out.Printf("%s\n", a.out.Yellow(fmt.Sprintf("not a number between %d and %d, left at %d", lo, hi, current)))
		return nil
	}
	*dst = n
	return nil
}

func (a *app) editScope(in *bufio.Reader, cfg *config.Config) error {
	return a.pick(in, "Which pull requests should quorum pick up?", cfg, []option{
		{"every repository that asks you", "any org, any repo, including your personal ones",
			func(c *config.Config) { c.Orgs, c.Repos = nil, nil },
			func(c config.Config) bool { return len(c.Orgs) == 0 && len(c.Repos) == 0 }},
		{"only certain organisations", "you will be asked which", nil,
			func(c config.Config) bool { return len(c.Orgs) > 0 }},
		{"only certain repositories", "you will be asked which", nil,
			func(c config.Config) bool { return len(c.Repos) > 0 }},
	}, func(choice int) error {
		switch choice {
		case 1:
			v, err := a.askText(in, "organisations, space separated", strings.Join(cfg.Orgs, " "))
			if err != nil {
				return err
			}
			cfg.Orgs, cfg.Repos = fieldsOrNil(v), nil
		case 2:
			v, err := a.askText(in, "repositories as owner/name, space separated", strings.Join(cfg.Repos, " "))
			if err != nil {
				return err
			}
			cfg.Repos, cfg.Orgs = fieldsOrNil(v), nil
		}
		return nil
	})
}

// skipLabel and skipEdit build the four "review these or not" rows, which are
// identical apart from the field they touch.
func skipLabel(skipped func(config.Config) bool) func(config.Config) string {
	return func(c config.Config) string {
		if skipped(c) {
			return "skipped"
		}
		return "reviewed"
	}
}

func skipEdit(title string, set func(*config.Config, bool), skipped func(config.Config) bool) func(*app, *bufio.Reader, *config.Config) error {
	return func(a *app, in *bufio.Reader, c *config.Config) error {
		return a.pick(in, title, c, []option{
			{"skip them", "", func(c *config.Config) { set(c, true) }, skipped},
			{"review them", "", func(c *config.Config) { set(c, false) },
				func(c config.Config) bool { return !skipped(c) }},
		}, nil)
	}
}

func orNone(s string) string {
	if strings.TrimSpace(s) == "" {
		return "none"
	}
	return s
}

func fieldsOrNil(s string) []string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return nil
	}
	return f
}

func trimNum(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// effortOptions builds the reasoning-effort picker for whichever field the
// caller points at.
func effortOptions(field func(*config.Config) *string) []option {
	var out []option
	for _, level := range codex.Efforts {
		out = append(out, option{level, "", func(c *config.Config) {
			*field(c) = level
		}, func(c config.Config) bool {
			cc := c
			return *field(&cc) == level
		}})
	}
	return out
}

// secs turns a configured interval in seconds into a duration.
func secs(n int) time.Duration { return time.Duration(n) * time.Second }

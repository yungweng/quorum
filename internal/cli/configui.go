package cli

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/engine"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/ui"
	"golang.org/x/term"
)

// setting is one editable row of `quorum config`. The screen shows only the
// settings worth changing regularly; the rarer knobs (resources, auto-merge
// wait and authors, the babysit extras) live in the config file alone, which
// `quorum config --path` locates.
type setting struct {
	name  string
	value func(config.Config) string
	edit  func(*app, *bufio.Reader, *config.Config) error
}

// settingGroup is one titled section of the config screen.
type settingGroup struct {
	title string
	items []setting
}

func (a *app) settingGroups() []settingGroup {
	return []settingGroup{
		{"agent", []setting{
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

			agentActionSetting(),
			autoMergeSetting(),
			notifySetting(),
			readyToMergeSetting(),
		}},

		{"review", []setting{
			{"review engine", func(c config.Config) string {
				return engineDesc(c.ReviewEngine)
			}, func(a *app, in *bufio.Reader, c *config.Config) error {
				return a.pick(in, "Which engine runs the reviewers, aggregator and verifier? (switching resets model and effort to the engine's default)", c,
					engineOptions(
						func(c *config.Config) *string { return &c.ReviewEngine },
						func(c *config.Config) *string { return &c.ReviewModel },
						func(c *config.Config) *string { return &c.ReviewEffort },
					), nil)
			}},

			{"review model", reviewModelDesc, func(a *app, in *bufio.Reader, c *config.Config) error {
				v, err := a.askText(in, fmt.Sprintf(
					"model for the reviewers, aggregator and verifier: %s (enter keeps, - resets to the engine's default)",
					modelHint(c.ReviewEngine)), c.ReviewModel)
				if err != nil {
					return err
				}
				c.ReviewModel = clearable(v)
				return a.pick(in, "How hard should the reviewers think?", c, defaultEffortOptions(
					c.ReviewEngine, func(c *config.Config) *string { return &c.ReviewEffort }), nil)
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
		}},

		{"fix", []setting{
			{"fix engine", func(c config.Config) string {
				return engineDesc(c.FixEngine)
			}, func(a *app, in *bufio.Reader, c *config.Config) error {
				return a.pick(in, "Which engine runs the fix sessions? (switching resets model and effort to the engine's default)", c,
					engineOptions(
						func(c *config.Config) *string { return &c.FixEngine },
						func(c *config.Config) *string { return &c.FixModel },
						func(c *config.Config) *string { return &c.FixEffort },
					), nil)
			}},

			{"fix model", fixModelDesc, func(a *app, in *bufio.Reader, c *config.Config) error {
				v, err := a.askText(in, fmt.Sprintf(
					"model for the fix sessions: %s (enter keeps, - resets to the engine's default)",
					modelHint(c.FixEngine)), c.FixModel)
				if err != nil {
					return err
				}
				c.FixModel = clearable(v)
				return a.pick(in, "How hard should the fix sessions think?", c, defaultEffortOptions(
					c.FixEngine, func(c *config.Config) *string { return &c.FixEffort }), nil)
			}},

			{"fix sessions", func(c config.Config) string {
				return fmt.Sprintf("up to %d rounds, %s per step", c.MaxIter, config.FormatDuration(c.FixTimeout))
			}, func(a *app, in *bufio.Reader, c *config.Config) error {
				n, err := a.askText(in, "maximum review to fix rounds", fmt.Sprint(c.MaxIter))
				if err != nil {
					return err
				}
				if i, err := strconv.Atoi(strings.TrimSpace(n)); err == nil && i >= 1 {
					c.MaxIter = i
				}
				return nil
			}},

			{"loop mode", func(c config.Config) string {
				if c.LoopMode == config.LoopOnline {
					return "online: push and wait for CI after every fix round"
				}
				return "offline: iterate locally, one push and CI run at the end"
			}, func(a *app, in *bufio.Reader, c *config.Config) error {
				return a.pick(in, "How should babysit iterate?", c, []option{
					{"offline", "review and fix local commits; one push and one CI run at the end",
						func(c *config.Config) { c.LoopMode = config.LoopOffline },
						func(c config.Config) bool { return c.LoopMode != config.LoopOnline }},
					{"online", "push and wait for CI after every fix round",
						func(c *config.Config) { c.LoopMode = config.LoopOnline },
						func(c config.Config) bool { return c.LoopMode == config.LoopOnline }},
				}, nil)
			}},
		}},
	}
}

// settings is the flat row list the cursor walks and the tests index.
func (a *app) settings() []setting {
	var out []setting
	for _, g := range a.settingGroups() {
		out = append(out, g.items...)
	}
	return out
}

// settingNameWidth is the name column, sized by the longest row name.
func settingNameWidth(groups []settingGroup) int {
	w := 0
	for _, g := range groups {
		for _, s := range g.items {
			w = max(w, ui.Cells(s.name))
		}
	}
	return w
}

func agentActionSetting() setting {
	return setting{"agent does", func(c config.Config) string {
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
	}}
}

func autoMergeSetting() setting {
	return setting{"auto-merge", func(c config.Config) string {
		if c.AutoMerge {
			return "approve clean PRs and ask GitHub to merge them"
		}
		return "off"
	}, func(a *app, in *bufio.Reader, c *config.Config) error {
		return a.pick(in, "Auto-merge a clean review? One switch for agent, review and babysit runs.", c, []option{
			{"off", "leave the pull request open", func(c *config.Config) { c.AutoMerge = false },
				func(c config.Config) bool { return !c.AutoMerge }},
			{"on", "approve and merge only the exact reviewed head", func(c *config.Config) { c.AutoMerge = true },
				func(c config.Config) bool { return c.AutoMerge }},
		}, nil)
	}}
}

func notifySetting() setting {
	return setting{"notifications", func(c config.Config) string {
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
	}}
}

func readyToMergeSetting() setting {
	return setting{"ready to merge", func(c config.Config) string {
		if c.NotifyReadyToMerge {
			return "clickable notification per clean PR not auto-merged"
		}
		return "off"
	}, func(a *app, in *bufio.Reader, c *config.Config) error {
		return a.pick(in, "Persistent alert when a clean review leaves a PR ready to merge?", c, []option{
			{"alert me", "one Notification Center item per PR; click to open it on GitHub",
				func(c *config.Config) { c.NotifyReadyToMerge = true },
				func(c config.Config) bool { return c.NotifyReadyToMerge }},
			{"off", "only the regular completion notification",
				func(c *config.Config) { c.NotifyReadyToMerge = false },
				func(c config.Config) bool { return !c.NotifyReadyToMerge }},
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
	groups := a.settingGroups()
	width := settingNameWidth(groups)
	w.Printf("%s\n", a.p.Config)
	for _, g := range groups {
		w.Printf("\n%s\n", w.Dim(g.title))
		for _, s := range g.items {
			w.Printf("  %s %s\n", ui.Pad(s.name, width), s.value(a.cfg))
		}
	}
}

// settingsMenu is the interactive editor. It keeps a working copy so leaving
// without saving really changes nothing.
func (a *app) settingsMenu() int {
	w := a.out
	in := bufio.NewReader(os.Stdin)
	cfg := a.cfg
	groups := a.settingGroups()
	items := a.settings()
	width := settingNameWidth(groups)
	cursor, dirty := 0, false
	// The rows, a title per group, a blank line between groups, and the
	// blank plus state line at the bottom.
	total := len(items) + 2*len(groups) + 1

	draw := func(first bool) {
		if !first {
			fmt.Fprintf(w.Out, "\x1b[%dA", total)
		}
		i := 0
		for gi, g := range groups {
			if gi > 0 {
				fmt.Fprintf(w.Out, "\r\x1b[K\r\n")
			}
			fmt.Fprintf(w.Out, "\r\x1b[K%s\r\n", w.Dim(g.title))
			for _, s := range g.items {
				pointer, name := "  ", ui.Pad(s.name, width)
				if i == cursor {
					pointer, name = w.Cyan("› "), w.Bold(ui.Pad(s.name, width))
				}
				fmt.Fprintf(w.Out, "\r\x1b[K%s%s %s\r\n", pointer, name, w.Dim(s.value(cfg)))
				i++
			}
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
			// The poll interval lives in the launchd job. Refresh now so the
			// user does not wait for the next poll's self-heal.
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

// clearable maps the literal "-" to an empty value. askText keeps the current
// value on empty input, so without this a once-set model could never go back
// to meaning "the engine's default".
func clearable(v string) string {
	if v = strings.TrimSpace(v); v == "-" {
		return ""
	}
	return v
}

// engineDesc names an engine for a settings row.
func engineDesc(name string) string {
	switch name {
	case config.EngineClaude:
		return "claude (Claude Code)"
	case config.EngineGrok:
		return "grok (Grok CLI)"
	default:
		return "codex"
	}
}

// engineName is the bare engine name, with the empty default resolved, for
// prompts that talk about one engine's own settings.
func engineName(name string) string {
	switch name {
	case config.EngineClaude:
		return config.EngineClaude
	case config.EngineGrok:
		return config.EngineGrok
	default:
		return config.EngineCodex
	}
}

// reviewModelDesc and fixModelDesc render what a run will really use. A plain
// "engine default" hid two different things behind one label: an empty review
// model is filled in by quorum before the engine ever sees it, while an empty
// fix model is left to the CLI's own choice, which quorum does not know.
func reviewModelDesc(c config.Config) string {
	_, model, effort := review.ReviewEngineDefaults(c.ReviewEngine, c.ReviewModel, c.ReviewEffort)
	if c.ReviewModel == "" {
		model += " (quorum's default)"
	}
	switch {
	case c.ReviewEffort == "" && effort == "":
		effort = engineName(c.ReviewEngine) + "'s own"
	case c.ReviewEffort == "":
		effort += " (quorum's default)"
	}
	return fmt.Sprintf("%s, effort %s", model, effort)
}

func fixModelDesc(c config.Config) string {
	eng := engineName(c.FixEngine)
	if c.FixModel == "" && c.FixEffort == "" {
		return eng + "'s own choice"
	}
	return fmt.Sprintf("%s, effort %s",
		orText(c.FixModel, eng+"'s own choice"), orText(c.FixEffort, eng+"'s own"))
}

// modelHint gives the model prompt the examples that engine accepts. The
// name spaces have nothing in common, and a model typed for the wrong engine
// is only found out when the run fails.
func modelHint(eng string) string {
	switch eng {
	case config.EngineClaude:
		return "an alias such as opus, sonnet or fable, or a full name such as claude-opus-5"
	case config.EngineGrok:
		return "a Grok model name such as " + review.GrokDefaultModel
	default:
		return "a Codex model name such as " + review.DefaultModel
	}
}

// engineOptions builds the engine picker for one engine field and the model
// and effort fields that belong to it. Switching the engine resets both:
// an explicit model was chosen for the engine it was typed under, and
// carrying it across would hand one engine the other's model name.
func engineOptions(engineField, modelField, effortField func(*config.Config) *string) []option {
	set := func(v string) func(*config.Config) {
		return func(c *config.Config) {
			if *engineField(c) == v {
				return
			}
			*engineField(c) = v
			*modelField(c) = ""
			*effortField(c) = ""
		}
	}
	is := func(v string) func(config.Config) bool {
		return func(c config.Config) bool {
			cc := c
			return *engineField(&cc) == v
		}
	}
	return []option{
		{"codex", "OpenAI's Codex CLI", set(config.EngineCodex), is(config.EngineCodex)},
		{"claude", "Anthropic's Claude Code CLI", set(config.EngineClaude), is(config.EngineClaude)},
		{"grok", "xAI's Grok CLI", set(config.EngineGrok), is(config.EngineGrok)},
	}
}

// effortOptions builds the reasoning-effort picker for whichever field the
// caller points at, offering only the levels eng actually accepts. Codex knows
// minimal and ultra, Claude does not, and offering a level the engine will
// silently drop is how a panel ends up running at the wrong setting.
func effortOptions(eng string, field func(*config.Config) *string) []option {
	var out []option
	for _, level := range engine.Efforts(eng) {
		out = append(out, option{level, "", func(c *config.Config) {
			*field(c) = level
		}, func(c config.Config) bool {
			cc := c
			return *field(&cc) == level
		}})
	}
	return out
}

func defaultEffortOptions(eng string, field func(*config.Config) *string) []option {
	out := []option{{
		label:  "your " + engineName(eng) + " default",
		detail: "do not override the reasoning effort",
		apply:  func(c *config.Config) { *field(c) = "" },
		match: func(c config.Config) bool {
			cc := c
			return *field(&cc) == ""
		},
	}}
	return append(out, effortOptions(eng, field)...)
}

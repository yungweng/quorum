// Package config reads and writes quorum's configuration file.
//
// The file is the same shell-syntax KEY=value file prbot used, so upgrading
// does not ask the user to migrate anything. It is parsed here rather than
// sourced: only assignments are understood, and anything else in the file is
// reported instead of executed.
package config

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/atomicfile"
	"github.com/yungweng/quorum/internal/engine/spec"
)

// Config is the fully resolved configuration: file values on top of defaults.
type Config struct {
	Orgs         []string
	Repos        []string
	ExcludeRepos []string

	IncludeTeams bool
	Teams        []string

	MaxRetries    int
	MaxConcurrent int // reviews running at once
	Reviewers     int // reviewer passes per review, all of them run in parallel
	LoadLimit     float64
	Nice          int
	CacheBudgetGB float64
	PollInterval  int

	// History is how many finished runs the dashboard lists. It is one entry
	// per run rather than per pull request, so several reviews of the same one
	// each take a line.
	History int

	SkipDrafts bool
	SkipForks  bool
	SkipBots   bool
	SkipOwn    bool

	// Review settings. An empty model or effort means the selected engine's
	// own default.
	ReviewEngine  string // codex, claude or grok
	ReviewModel   string
	ReviewEffort  string
	ReviewTimeout time.Duration
	Post          bool // false is the old REVIEW_ARGS="--dry-run"

	// Babysit settings for the fix sessions.
	FixEngine  string // codex, claude or grok
	FixModel   string
	FixEffort  string
	MaxIter    int
	MaxCIFixes int
	FixTimeout time.Duration
	// DivergenceScan runs one read-only analysis after MaxIter is exhausted.
	// Escalation targets are GitHub users or org/team slugs without a leading @.
	DivergenceScan       bool
	DivergenceEscalateTo []string
	// Sandboxed opts the fix sessions out of the engine's sandbox and
	// permission bypass. They then run under the user's own engine config,
	// which must allow commands, network and push or every fix round fails.
	Sandboxed bool
	// BabysitDrafts lets `quorum babysit` work on a draft pull request without
	// passing --draft each time.
	BabysitDrafts bool
	// ResolveConflicts has babysit merge the latest base before reviewing and
	// resolve conflicts through the fix session.
	ResolveConflicts bool
	// BaseUpdate is when ResolveConflicts merges the base: baseUpdateAlways
	// (the default) whenever the branch is behind, BaseUpdateConflicts only
	// when the merge would conflict.
	BaseUpdate string
	// FixSuggestions has babysit run one terminal fix round when the final
	// review is clean but still lists Suggestions: triage each one, implement
	// what is worth it, and end without another review.
	FixSuggestions bool
	// LoopMode is how babysit iterates. "offline" (the default) keeps every
	// review-fix round local - reviews of the unpushed worktree head, a local
	// test command instead of a per-round CI wait - and pushes once at the
	// end, triggering a single CI run. "online" is the old behaviour: push and
	// wait for CI after every fix round.
	LoopMode string

	// AgentAction is what the daemon does with a pull request that asks for a
	// review: "review" posts one and stops, "babysit" runs the full fix loop.
	AgentAction string

	// AutoMerge is one opt-in for every source: agent runs, `quorum review`
	// and `quorum babysit` alike. AutoMergeTimeout bounds the wait for
	// checks and mergeability; zero leaves it bounded only by the run context.
	AutoMerge        bool
	AutoMergeTimeout time.Duration
	// AutoMergeAuthors limits auto-merge to pull requests whose author is one
	// of these GitHub logins, compared case-insensitively. Empty lets every
	// author with a clean review qualify.
	AutoMergeAuthors []string

	Notify bool
	// NotifyReadyToMerge leaves one clickable Notification Center
	// item per pull request whose clean review was not auto-merged, so "ready
	// to merge" survives until the user opens the PR or dismisses it.
	NotifyReadyToMerge bool

	// reviewArgs is the retired pass-through string. It is still read so an
	// existing config keeps working, and dropped when the file is rewritten.
	reviewArgs string

	// unknown keeps assignments quorum does not recognise so that rewriting the
	// file never silently drops something the user put there.
	unknown map[string]string
}

// MaxReviewers bounds REVIEWERS. It is the review core's own guard against a
// typo costing a fortune in reviewer passes, repeated here so the config says
// so when it is read instead of every run failing later.
const MaxReviewers = 12

// Agent actions.
const (
	ActionReview  = "review"
	ActionBabysit = "babysit"
)

// Babysit loop modes.
const (
	LoopOffline = "offline"
	LoopOnline  = "online"
)

// Base update modes.
const (
	baseUpdateAlways    = "always"
	BaseUpdateConflicts = "conflicts"
)

// LoadLimit is off by default: the point of the agent is that a review starts
// when it is requested, and MaxConcurrent is the honest place to say how many
// of them you want at once.
func Default() Config {
	return Config{
		IncludeTeams:  true,
		MaxRetries:    3,
		MaxConcurrent: 6,
		Reviewers:     6,
		LoadLimit:     0,
		Nice:          10,
		CacheBudgetGB: 5,
		PollInterval:  300,
		History:       20,
		SkipDrafts:    true,
		SkipForks:     true,
		SkipBots:      true,
		SkipOwn:       true,

		// Model and effort stay empty on purpose: empty means the selected
		// engine's own default, so switching REVIEW_ENGINE never feeds one
		// engine another engine's model name.
		ReviewEngine:  spec.Codex,
		ReviewTimeout: 45 * time.Minute,
		Post:          true,

		FixEngine:        spec.Codex,
		MaxIter:          12,
		MaxCIFixes:       3,
		FixTimeout:       2 * time.Hour,
		ResolveConflicts: true,
		BaseUpdate:       baseUpdateAlways,
		FixSuggestions:   true,
		LoopMode:         LoopOffline,

		AgentAction:      ActionReview,
		AutoMergeTimeout: 2 * time.Hour,

		Notify:  true,
		unknown: map[string]string{},
	}
}

// ParseDuration accepts what the shell tools accepted: a bare number of
// seconds, or a value like 30m, 45m, 2h. Zero disables the timeout it belongs
// to, which is why it is a valid value rather than an error.
func ParseDuration(v string) (time.Duration, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return 0, fmt.Errorf("empty duration")
	}
	if n, err := strconv.Atoi(s); err == nil {
		if n < 0 {
			return 0, fmt.Errorf("duration must not be negative")
		}
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("expected seconds or a duration like 30m, 45m, 1h, or 0")
	}
	return d, nil
}

func FormatDuration(d time.Duration) string {
	switch {
	case d == 0:
		return "0"
	case d%time.Hour == 0:
		return strconv.Itoa(int(d/time.Hour)) + "h"
	case d%time.Minute == 0:
		return strconv.Itoa(int(d/time.Minute)) + "m"
	default:
		return strconv.Itoa(int(d/time.Second)) + "s"
	}
}

// Codex is the largest number of Codex processes that can be running at once:
// every review runs all of its reviewer passes in parallel, because a review
// that trickles through its passes is a review you end up waiting for.
func (c Config) Codex() int {
	return max(c.MaxConcurrent, 1) * max(c.Reviewers, 1)
}

// Load reads path on top of the defaults. A missing file is not an error: it
// means "everything default", which is a working configuration.
func Load(path string) (Config, error) {
	cfg := Default()
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return cfg, err
	}
	defer f.Close()

	var problems []string
	sc := bufio.NewScanner(f)
	line := 0
	for sc.Scan() {
		line++
		key, value, ok := parseLine(sc.Text())
		if !ok {
			if key != "" {
				problems = append(problems, fmt.Sprintf("line %d: %s", line, key))
			}
			continue
		}
		if problem := cfg.set(key, value); problem != "" {
			problems = append(problems, fmt.Sprintf("line %d: %s", line, problem))
		}
	}
	if err := sc.Err(); err != nil {
		return cfg, err
	}
	// An effort the configured engine does not know is dropped here instead of
	// failing the run later. The two level sets are engine-specific, but the
	// file may predate that and pair a codex level with the claude engine, so
	// the alternative is a tool that refuses to start on its own config - with
	// no terminal to say so behind the launchd agent. It is reported like any
	// other value that was not used, and the run headers name the resolved
	// model and effort.
	for _, e := range []struct {
		key, engine string
		effort      *string
	}{
		{"REVIEW_EFFORT", cfg.ReviewEngine, &cfg.ReviewEffort},
		{"FIX_EFFORT", cfg.FixEngine, &cfg.FixEffort},
	} {
		if known := spec.KnownEffort(e.engine, *e.effort); known != *e.effort {
			problems = append(problems, fmt.Sprintf("%s=%q ignored, %s takes %s",
				e.key, *e.effort, e.engine, strings.Join(spec.Efforts(e.engine), ", ")))
			*e.effort = known
		}
	}
	if len(problems) > 0 {
		return cfg, fmt.Errorf("%s: %s", path, strings.Join(problems, "; "))
	}
	return cfg, nil
}

// parseLine returns key and value for an assignment. For a line that is neither
// blank, a comment, nor an assignment, ok is false and key carries a complaint.
func parseLine(raw string) (key, value string, ok bool) {
	s := strings.TrimSpace(raw)
	if s == "" || strings.HasPrefix(s, "#") {
		return "", "", false
	}
	eq := strings.Index(s, "=")
	if eq <= 0 {
		return "not an assignment: " + s, "", false
	}
	key = strings.TrimSpace(s[:eq])
	if strings.ContainsAny(key, " \t") {
		return "not an assignment: " + s, "", false
	}
	return key, unquote(s[eq+1:]), true
}

// unquote takes the right-hand side of an assignment and returns its value:
// quotes removed, and a trailing comment stripped only when it sits outside
// quotes, so REVIEW_ARGS="--foo #bar" keeps its hash.
func unquote(rhs string) string {
	rhs = strings.TrimLeft(rhs, " \t")
	var b strings.Builder
	quote := byte(0)
	for i := 0; i < len(rhs); i++ {
		c := rhs[i]
		switch {
		case quote != 0 && c == quote:
			quote = 0
		case quote != 0:
			b.WriteByte(c)
		case c == '\'' || c == '"':
			quote = c
		case c == '#':
			return strings.TrimRight(b.String(), " \t")
		default:
			b.WriteByte(c)
		}
	}
	return strings.TrimRight(b.String(), " \t")
}

// set applies one assignment. A value it cannot use leaves the setting as it
// was and comes back as a complaint: silently falling back turned
// AGENT_ACTION=babysitt into plain reviews and let REVIEWERS=20 load only to
// fail every run.
func (c *Config) set(key, value string) (problem string) {
	// An empty value was never a typo: it leaves the default in place, and
	// switches a boolean off.
	empty := strings.TrimSpace(value) == ""
	reject := func(want string) {
		if !empty {
			problem = fmt.Sprintf("%s=%q ignored, expected %s", key, value, want)
		}
	}
	boolean := func(dst *bool) {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case "1", "true", "yes", "on":
			*dst = true
		case "0", "false", "no", "off", "":
			*dst = false
		default:
			reject("1 or 0")
		}
	}
	// hi below lo means no upper bound.
	integer := func(dst *int, lo, hi int) {
		n, err := strconv.Atoi(strings.TrimSpace(value))
		switch {
		case err != nil:
			reject("a number")
		case n < lo && hi < lo:
			reject(fmt.Sprintf("%d or more", lo))
		case n < lo || hi >= lo && n > hi:
			reject(fmt.Sprintf("%d to %d", lo, hi))
		default:
			*dst = n
		}
	}
	float := func(dst *float64) {
		n, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil || n < 0 {
			reject("a number, 0 or more")
			return
		}
		*dst = n
	}
	duration := func(dst *time.Duration) {
		d, err := ParseDuration(value)
		if err != nil {
			reject("seconds or a duration like 30m, 2h, or 0")
			return
		}
		*dst = d
	}
	choice := func(dst *string, options ...string) {
		v := strings.TrimSpace(strings.ToLower(value))
		if !slices.Contains(options, v) {
			reject(strings.Join(options, " or "))
			return
		}
		*dst = v
	}
	engines := make([]string, 0, len(spec.All))
	for _, e := range spec.All {
		engines = append(engines, e.Name)
	}

	switch key {
	case "ORGS":
		c.Orgs = fields(value)
	case "REPOS":
		c.Repos = fields(value)
	case "EXCLUDE_REPOS":
		c.ExcludeRepos = fields(value)
	case "TEAMS":
		c.Teams = fields(value)
	case "INCLUDE_TEAMS":
		boolean(&c.IncludeTeams)
	case "SKIP_DRAFTS":
		boolean(&c.SkipDrafts)
	case "SKIP_FORKS":
		boolean(&c.SkipForks)
	case "SKIP_BOTS":
		boolean(&c.SkipBots)
	case "SKIP_OWN":
		boolean(&c.SkipOwn)
	case "NOTIFY":
		boolean(&c.Notify)
	case "NOTIFY_READY_TO_MERGE":
		boolean(&c.NotifyReadyToMerge)
	case "MAX_RETRIES":
		integer(&c.MaxRetries, 0, -1)
	case "MAX_CONCURRENT":
		integer(&c.MaxConcurrent, 1, 0)
	case "MAX_CODEX":
		// Retired: it used to divide the reviewer passes between running
		// reviews, which only made every review slower. Accepted so an older
		// config still loads, and dropped when the file is written again.
	case "REVIEWERS":
		integer(&c.Reviewers, 1, MaxReviewers)
	case "NICE":
		integer(&c.Nice, math.MinInt, math.MaxInt)
	case "HISTORY":
		integer(&c.History, 0, -1)
	case "POLL_INTERVAL":
		integer(&c.PollInterval, 1, 0)
	case "LOAD_LIMIT":
		float(&c.LoadLimit)
	case "CACHE_BUDGET_GB":
		float(&c.CacheBudgetGB)
	case "REVIEW_ENGINE":
		choice(&c.ReviewEngine, engines...)
	case "REVIEW_MODEL":
		c.ReviewModel = strings.TrimSpace(value)
	case "REVIEW_EFFORT":
		c.ReviewEffort = strings.TrimSpace(value)
	case "REVIEW_TIMEOUT":
		duration(&c.ReviewTimeout)
	case "POST":
		boolean(&c.Post)
	case "FIX_ENGINE":
		choice(&c.FixEngine, engines...)
	case "FIX_MODEL":
		c.FixModel = strings.TrimSpace(value)
	case "FIX_EFFORT":
		c.FixEffort = strings.TrimSpace(value)
	case "MAX_ITER":
		integer(&c.MaxIter, 1, 0)
	case "MAX_CI_FIXES":
		integer(&c.MaxCIFixes, 0, -1)
	case "FIX_TIMEOUT":
		duration(&c.FixTimeout)
	case "DIVERGENCE_SCAN":
		boolean(&c.DivergenceScan)
	case "DIVERGENCE_ESCALATE_TO":
		c.DivergenceEscalateTo = fields(value)
	case "SANDBOXED":
		boolean(&c.Sandboxed)
	case "BABYSIT_DRAFTS":
		boolean(&c.BabysitDrafts)
	case "RESOLVE_CONFLICTS":
		boolean(&c.ResolveConflicts)
	case "BASE_UPDATE":
		choice(&c.BaseUpdate, baseUpdateAlways, BaseUpdateConflicts)
	case "FIX_SUGGESTIONS":
		boolean(&c.FixSuggestions)
	case "LOOP_MODE":
		choice(&c.LoopMode, LoopOffline, LoopOnline)
	case "AGENT_ACTION":
		choice(&c.AgentAction, ActionReview, ActionBabysit)
	case "AUTO_MERGE":
		boolean(&c.AutoMerge)
	case "AUTO_MERGE_AGENT":
		// Retired: auto-merge used to be a separate opt-in per source. The
		// agent value migrates because unattended runs are where the setting
		// matters most; the other two keys are read no more and dropped when
		// the file is written again.
		boolean(&c.AutoMerge)
	case "AUTO_MERGE_REVIEW", "AUTO_MERGE_BABYSIT":
	case "AUTO_MERGE_TIMEOUT":
		duration(&c.AutoMergeTimeout)
	case "AUTO_MERGE_AUTHORS":
		c.AutoMergeAuthors = fields(value)
	case "REVIEW_ARGS":
		// Retired: it was passed verbatim to a separate binary that no longer
		// exists. The only flag anyone used through it was --dry-run, so that
		// one keeps working; the rest is preserved for the user to see and
		// migrate rather than silently ignored.
		c.reviewArgs = value
		if strings.Contains(value, "--dry-run") {
			c.Post = false
		}
	default:
		if c.unknown == nil {
			c.unknown = map[string]string{}
		}
		c.unknown[key] = value
	}
	return problem
}

// fields splits a space separated list, collapsing "unset" and "set to empty"
// into the same nil so the two are never distinguishable downstream.
func fields(v string) []string {
	f := strings.Fields(v)
	if len(f) == 0 {
		return nil
	}
	return f
}

func bit(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

func num(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// render produces the config file text: the current values with the comments
// that explain them, plus any assignment quorum did not recognise.
func (c Config) render() string {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }

	w("# quorum configuration. One KEY=value per line, # starts a comment.\n")
	w("# Edit by hand or run: quorum config\n\n")

	w("# Scope. Empty means every review request assigned to you, anywhere.\n")
	w("ORGS=%q\n", strings.Join(c.Orgs, " "))
	w("REPOS=%q\n", strings.Join(c.Repos, " "))
	w("EXCLUDE_REPOS=%q\n\n", strings.Join(c.ExcludeRepos, " "))

	w("# Requests aimed at a team you belong to. GitHub's review-requested:\n")
	w("# qualifier does not match those, so they are searched separately.\n")
	w("INCLUDE_TEAMS=%s\n", bit(c.IncludeTeams))
	w("TEAMS=%q\t# empty discovers your teams automatically\n\n", strings.Join(c.Teams, " "))

	w("# How much runs at once. Every review runs all REVIEWERS passes in\n")
	w("# parallel, so at peak there are MAX_CONCURRENT x REVIEWERS = %d Codex\n", c.Codex())
	w("# processes. They are mostly waiting on the network, not on the CPU.\n")
	w("MAX_CONCURRENT=%d\n", c.MaxConcurrent)
	w("REVIEWERS=%d\n", c.Reviewers)
	w("NICE=%d\t\t\t# scheduling priority for reviews, 0 disables\n", c.Nice)
	w("LOAD_LIMIT=%s\t\t# hold reviews back above this 1-minute load, 0 disables\n", num(c.LoadLimit))
	w("CACHE_BUDGET_GB=%s\t# runs and dependency trees together, 0 disables\n\n", num(c.CacheBudgetGB))

	w("MAX_RETRIES=%d\n", c.MaxRetries)
	w("POLL_INTERVAL=%d\t\t# next poll reloads the agent job when this changes\n", c.PollInterval)
	w("HISTORY=%d\t\t\t# finished runs listed by status and watch\n\n", c.History)

	w("# Safety. Fork and bot PRs run foreign code locally through direnv.\n")
	w("SKIP_DRAFTS=%s\n", bit(c.SkipDrafts))
	w("SKIP_FORKS=%s\n", bit(c.SkipForks))
	w("SKIP_BOTS=%s\n", bit(c.SkipBots))
	w("SKIP_OWN=%s\t\t\t# your own PRs; review one on purpose with `quorum run`\n\n", bit(c.SkipOwn))

	w("# Reviews. POST=0 keeps review output on disk, disables every PR write,\n")
	w("# and skips babysit's final description generation.\n")
	w("# An empty model or effort means the engine's own default. The effort\n")
	w("# levels differ per engine. A level the engine does not know is ignored\n")
	w("# in favour of the engine's default; quorum doctor and status report it.\n")
	w("REVIEW_ENGINE=%q\t# codex, claude or grok\n", c.ReviewEngine)
	w("REVIEW_MODEL=%q\n", c.ReviewModel)
	w("REVIEW_EFFORT=%q\t# codex: minimal..ultra, claude: low..max, grok: low..high\n", c.ReviewEffort)
	w("REVIEW_TIMEOUT=%q\t# per reviewer pass, 0 disables\n", FormatDuration(c.ReviewTimeout))
	w("POST=%s\n\n", bit(c.Post))

	w("# Babysit: the fix sessions that work through review findings.\n")
	w("# Unlike the reviews, nothing here is filled in: an empty model or effort\n")
	w("# leaves the choice to the selected engine's own CLI.\n")
	w("FIX_ENGINE=%q\t# codex, claude or grok\n", c.FixEngine)
	w("FIX_MODEL=%q\n", c.FixModel)
	w("FIX_EFFORT=%q\t# codex: minimal..ultra, claude: low..max, grok: low..high\n", c.FixEffort)
	w("MAX_ITER=%d\t\t# review -> fix rounds before giving up\n", c.MaxIter)
	w("MAX_CI_FIXES=%d\t\t# CI fix attempts per green-CI phase\n", c.MaxCIFixes)
	w("FIX_TIMEOUT=%q\t\t# per fix step; keep above your CI runtime\n", FormatDuration(c.FixTimeout))
	w("DIVERGENCE_SCAN=%s\t# analyze the round history after MAX_ITER\n", bit(c.DivergenceScan))
	w("DIVERGENCE_ESCALATE_TO=%q\t# users or org/team slugs, without @\n",
		strings.Join(c.DivergenceEscalateTo, " "))
	w("SANDBOXED=%s\t\t# 1 uses the engine's own sandbox/approval defaults instead of bypassing them\n", bit(c.Sandboxed))
	w("BABYSIT_DRAFTS=%s\t# 1 lets `quorum babysit` work on draft PRs without --draft\n", bit(c.BabysitDrafts))
	w("RESOLVE_CONFLICTS=%s\t# update from the base and resolve conflicts before reporting ready\n", bit(c.ResolveConflicts))
	w("BASE_UPDATE=%q\t# always: merge the base whenever behind; conflicts: only when it would conflict\n", c.BaseUpdate)
	w("FIX_SUGGESTIONS=%s\t# after a clean final review, triage and implement leftover Suggestions once\n", bit(c.FixSuggestions))
	w("LOOP_MODE=%q\t# offline: iterate locally, one push and CI run at the end; online: push and wait for CI every round\n\n", c.LoopMode)

	w("# What the agent does with a pull request that asks for your review:\n")
	w("# \"review\" posts a review and stops, \"babysit\" runs the full fix loop.\n")
	w("AGENT_ACTION=%q\n\n", c.AgentAction)

	w("# Approve a clean reviewed head and ask GitHub to merge it with a merge\n")
	w("# commit once its branch rules pass. One switch for agent, review and\n")
	w("# babysit runs alike.\n")
	w("AUTO_MERGE=%s\n", bit(c.AutoMerge))
	w("AUTO_MERGE_TIMEOUT=%q\t# wait for checks and mergeability; 0 disables timeout\n", FormatDuration(c.AutoMergeTimeout))
	w("AUTO_MERGE_AUTHORS=%q\t# only merge PRs from these logins; empty allows every author\n\n",
		strings.Join(c.AutoMergeAuthors, " "))

	w("NOTIFY=%s\n", bit(c.Notify))
	w("NOTIFY_READY_TO_MERGE=%s\t# clickable notification per clean PR not auto-merged\n", bit(c.NotifyReadyToMerge))

	if c.reviewArgs != "" {
		w("\n# Retired: pr-codex-review is built in now, so this is no longer passed\n")
		w("# anywhere. Use REVIEW_MODEL, REVIEW_EFFORT, REVIEWERS and POST instead.\n")
		w("# REVIEW_ARGS=%q\n", c.reviewArgs)
	}

	if len(c.unknown) > 0 {
		keys := make([]string, 0, len(c.unknown))
		for k := range c.unknown {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w("\n# Kept as written; quorum does not use these.\n")
		for _, k := range keys {
			w("%s=%q\n", k, c.unknown[k])
		}
	}
	return b.String()
}

func (c Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicfile.Write(path, []byte(c.render()))
}

// Package config reads and writes quorum's configuration file.
//
// The file is the same shell-syntax KEY=value file prbot used, so upgrading
// does not ask the user to migrate anything. It now also carries the review and
// babysit settings that used to be command-line flags on two separate tools. It is parsed
// here rather than sourced: only assignments are understood, and anything else
// in the file is reported instead of executed.
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/engine"
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

	// Review settings. These used to be flags on a separate binary that the
	// daemon passed through as an opaque REVIEW_ARGS string. An empty model
	// or effort means the selected engine's own default.
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
	// Sandboxed opts the fix sessions out of
	// --dangerously-bypass-approvals-and-sandbox. They then run under the
	// user's own codex config, which must allow commands, network and push or
	// every fix round fails.
	Sandboxed bool
	// BabysitDrafts lets `quorum babysit` work on a draft pull request without
	// passing --draft each time.
	BabysitDrafts bool
	// ResolveConflicts has babysit merge the base branch and resolve the
	// conflicts before any review round, instead of polishing a branch that
	// cannot land.
	ResolveConflicts bool

	// AgentAction is what the daemon does with a pull request that asks for a
	// review: "review" posts one and stops, "babysit" runs the full fix loop.
	// Only possible now that both live in one binary.
	AgentAction string

	// Auto-merge is opt-in per source. An agent run uses AutoMergeAgent even
	// when AgentAction is "babysit"; the other two fields only affect commands
	// started explicitly in a terminal. AutoMergeTimeout bounds the wait for
	// checks and mergeability; zero leaves it bounded only by the run context.
	AutoMergeAgent   bool
	AutoMergeReview  bool
	AutoMergeBabysit bool
	AutoMergeTimeout time.Duration
	// AutoMergeAuthors limits auto-merge to pull requests whose author is one
	// of these GitHub logins, compared case-insensitively. Empty keeps the old
	// behaviour: every author with a clean review qualifies.
	AutoMergeAuthors []string

	Notify bool

	// ReviewArgs is the retired pass-through string. It is still read so an
	// existing config keeps working, and dropped when the file is rewritten.
	ReviewArgs string

	// Unknown keeps assignments quorum does not recognise so that rewriting the
	// file never silently drops something the user put there.
	Unknown map[string]string
}

// Agent actions.
const (
	ActionReview  = "review"
	ActionBabysit = "babysit"
)

// The engines reviews and fix sessions can run on. Defined here rather than
// imported so config keeps its zero internal dependencies.
const (
	EngineCodex  = "codex"
	EngineClaude = "claude"
	EngineGrok   = "grok"
)

// knownEngine reports whether v is a recognised engine name.
func knownEngine(v string) bool {
	return v == EngineCodex || v == EngineClaude || v == EngineGrok
}

// Default mirrors the defaults the shell version shipped, with the resource
// limits added. LoadLimit is off by default: the point of the agent is that a
// review starts when it is requested, and MaxConcurrent is the honest place to
// say how many of them you want at once.
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
		ReviewEngine:  EngineCodex,
		ReviewTimeout: 45 * time.Minute,
		Post:          true,

		FixEngine:        EngineCodex,
		MaxIter:          12,
		MaxCIFixes:       3,
		FixTimeout:       2 * time.Hour,
		ResolveConflicts: true,

		AgentAction:      ActionReview,
		AutoMergeTimeout: 2 * time.Hour,

		Notify:  true,
		Unknown: map[string]string{},
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

// FormatDuration renders a duration the way the config file writes it.
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
		cfg.set(key, value)
	}
	if err := sc.Err(); err != nil {
		return cfg, err
	}
	// An effort the configured engine does not know is dropped here instead of
	// failing the run later. The two level sets are engine-specific, but the
	// file may predate that and pair a codex level with the claude engine, so
	// the alternative is a tool that refuses to start on its own config - with
	// no terminal to say so behind the launchd agent. What the run then uses is
	// still visible: the run headers name the resolved model and effort.
	cfg.ReviewEffort = engine.KnownEffort(cfg.ReviewEngine, cfg.ReviewEffort)
	cfg.FixEffort = engine.KnownEffort(cfg.FixEngine, cfg.FixEffort)
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

func (c *Config) set(key, value string) {
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
		c.IncludeTeams = truthy(value)
	case "SKIP_DRAFTS":
		c.SkipDrafts = truthy(value)
	case "SKIP_FORKS":
		c.SkipForks = truthy(value)
	case "SKIP_BOTS":
		c.SkipBots = truthy(value)
	case "SKIP_OWN":
		c.SkipOwn = truthy(value)
	case "NOTIFY":
		c.Notify = truthy(value)
	case "MAX_RETRIES":
		c.MaxRetries = intOr(value, c.MaxRetries)
	case "MAX_CONCURRENT":
		c.MaxConcurrent = intOr(value, c.MaxConcurrent)
	case "MAX_CODEX":
		// Retired: it used to divide the reviewer passes between running
		// reviews, which only made every review slower. Accepted so an older
		// config still loads, and dropped when the file is written again.
	case "REVIEWERS":
		c.Reviewers = intOr(value, c.Reviewers)
	case "NICE":
		c.Nice = intOr(value, c.Nice)
	case "HISTORY":
		c.History = intOr(value, c.History)
	case "POLL_INTERVAL":
		c.PollInterval = intOr(value, c.PollInterval)
	case "LOAD_LIMIT":
		c.LoadLimit = floatOr(value, c.LoadLimit)
	case "CACHE_BUDGET_GB":
		c.CacheBudgetGB = floatOr(value, c.CacheBudgetGB)
	case "REVIEW_ENGINE":
		if v := strings.TrimSpace(strings.ToLower(value)); knownEngine(v) {
			c.ReviewEngine = v
		}
	case "REVIEW_MODEL":
		c.ReviewModel = strings.TrimSpace(value)
	case "REVIEW_EFFORT":
		c.ReviewEffort = strings.TrimSpace(value)
	case "REVIEW_TIMEOUT":
		c.ReviewTimeout = durationOr(value, c.ReviewTimeout)
	case "POST":
		c.Post = truthy(value)
	case "FIX_ENGINE":
		if v := strings.TrimSpace(strings.ToLower(value)); knownEngine(v) {
			c.FixEngine = v
		}
	case "FIX_MODEL":
		c.FixModel = strings.TrimSpace(value)
	case "FIX_EFFORT":
		c.FixEffort = strings.TrimSpace(value)
	case "MAX_ITER":
		c.MaxIter = intOr(value, c.MaxIter)
	case "MAX_CI_FIXES":
		c.MaxCIFixes = intOr(value, c.MaxCIFixes)
	case "FIX_TIMEOUT":
		c.FixTimeout = durationOr(value, c.FixTimeout)
	case "DIVERGENCE_SCAN":
		c.DivergenceScan = truthy(value)
	case "DIVERGENCE_ESCALATE_TO":
		c.DivergenceEscalateTo = fields(value)
	case "SANDBOXED":
		c.Sandboxed = truthy(value)
	case "BABYSIT_DRAFTS":
		c.BabysitDrafts = truthy(value)
	case "RESOLVE_CONFLICTS":
		c.ResolveConflicts = truthy(value)
	case "AGENT_ACTION":
		if v := strings.TrimSpace(strings.ToLower(value)); v == ActionReview || v == ActionBabysit {
			c.AgentAction = v
		}
	case "AUTO_MERGE_AGENT":
		c.AutoMergeAgent = truthy(value)
	case "AUTO_MERGE_REVIEW":
		c.AutoMergeReview = truthy(value)
	case "AUTO_MERGE_BABYSIT":
		c.AutoMergeBabysit = truthy(value)
	case "AUTO_MERGE_TIMEOUT":
		c.AutoMergeTimeout = durationOr(value, c.AutoMergeTimeout)
	case "AUTO_MERGE_AUTHORS":
		c.AutoMergeAuthors = fields(value)
	case "REVIEW_ARGS":
		// Retired: it was passed verbatim to a separate binary that no longer
		// exists. The only flag anyone used through it was --dry-run, so that
		// one keeps working; the rest is preserved for the user to see and
		// migrate rather than silently ignored.
		c.ReviewArgs = value
		if strings.Contains(value, "--dry-run") {
			c.Post = false
		}
	default:
		if c.Unknown == nil {
			c.Unknown = map[string]string{}
		}
		c.Unknown[key] = value
	}
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

// truthy accepts the shell habit of 1/0 as well as the words, so a config
// edited by hand behaves the way its author expected.
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func intOr(v string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil {
		return fallback
	}
	return n
}

func durationOr(v string, fallback time.Duration) time.Duration {
	d, err := ParseDuration(v)
	if err != nil {
		return fallback
	}
	return d
}

func floatOr(v string, fallback float64) float64 {
	n, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return fallback
	}
	return n
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

// Render produces the config file text: the current values with the comments
// that explain them, plus any assignment quorum did not recognise.
func (c Config) Render() string {
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
	w("# levels differ per engine, and a level the engine does not know is\n")
	w("# refused rather than silently dropped.\n")
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
	w("RESOLVE_CONFLICTS=%s\t# merge the base branch and resolve conflicts before reviewing\n\n", bit(c.ResolveConflicts))

	w("# What the agent does with a pull request that asks for your review:\n")
	w("# \"review\" posts a review and stops, \"babysit\" runs the full fix loop.\n")
	w("AGENT_ACTION=%q\n\n", c.AgentAction)

	w("# Approve a clean reviewed head and ask GitHub to merge it with a merge\n")
	w("# commit once its branch rules pass. Each source is opt-in.\n")
	w("AUTO_MERGE_AGENT=%s\n", bit(c.AutoMergeAgent))
	w("AUTO_MERGE_REVIEW=%s\n", bit(c.AutoMergeReview))
	w("AUTO_MERGE_BABYSIT=%s\n", bit(c.AutoMergeBabysit))
	w("AUTO_MERGE_TIMEOUT=%q\t# wait for checks and mergeability; 0 disables timeout\n", FormatDuration(c.AutoMergeTimeout))
	w("AUTO_MERGE_AUTHORS=%q\t# only merge PRs from these logins; empty allows every author\n\n",
		strings.Join(c.AutoMergeAuthors, " "))

	w("NOTIFY=%s\n", bit(c.Notify))

	if c.ReviewArgs != "" {
		w("\n# Retired: pr-codex-review is built in now, so this is no longer passed\n")
		w("# anywhere. Use REVIEW_MODEL, REVIEW_EFFORT, REVIEWERS and POST instead.\n")
		w("# REVIEW_ARGS=%q\n", c.ReviewArgs)
	}

	if len(c.Unknown) > 0 {
		keys := make([]string, 0, len(c.Unknown))
		for k := range c.Unknown {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		w("\n# Kept as written; quorum does not use these.\n")
		for _, k := range keys {
			w("%s=%q\n", k, c.Unknown[k])
		}
	}
	return b.String()
}

// Save writes the config atomically, creating the directory if needed.
func (c Config) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(c.Render()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

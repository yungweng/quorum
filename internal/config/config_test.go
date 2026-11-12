package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Values are read the way the shell read them: quotes removed, and a trailing
// comment stripped only outside quotes. A key quorum does not know is kept and
// written back verbatim, which shows the parsed value without reaching into
// the struct.
func TestLoadReadsValuesLikeTheShell(t *testing.T) {
	cases := []struct{ in, want string }{
		{`""`, ""},
		{`"acme-inc myorg"`, "acme-inc myorg"},
		{`acme-inc`, "acme-inc"},
		{`1`, "1"},
		{`"acme"   # trailing comment`, "acme"},
		{`acme   # trailing comment`, "acme"},
		{`"--dry-run -n 4"`, "--dry-run -n 4"},
		{`"--grep #hash"`, "--grep #hash"}, // a hash inside quotes is a value
		{`'single quoted'`, "single quoted"},
		{`   spaced   `, "spaced"},
	}
	dir := t.TempDir()
	for _, c := range cases {
		in, out := filepath.Join(dir, "in"), filepath.Join(dir, "out")
		if err := os.WriteFile(in, []byte("FUTURE_KEY="+c.in+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := Load(in)
		if err != nil {
			t.Fatalf("Load(FUTURE_KEY=%s): %v", c.in, err)
		}
		if err := cfg.Save(out); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		if want := "\nFUTURE_KEY=" + strconv.Quote(c.want) + "\n"; !strings.Contains(string(b), want) {
			t.Errorf("FUTURE_KEY=%s was written back without %q:\n%s", c.in, strings.TrimSpace(want), b)
		}
	}
}

// The config file shipped by the shell version has to keep working untouched.
func TestLoadShellEraConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	legacy := `# prbot configuration. Shell syntax, sourced by the script.

ORGS=""                  # e.g. "acme-inc myorg"
REPOS=""
EXCLUDE_REPOS="acme-inc/legacy other/repo"

INCLUDE_TEAMS=1
TEAMS=""

MAX_RETRIES=3
MAX_CONCURRENT=3
POLL_INTERVAL=300

SKIP_DRAFTS=1
SKIP_FORKS=1
SKIP_BOTS=0
SKIP_OWN=1

REVIEW_ARGS="--dry-run"

NOTIFY=1
`
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !reflect.DeepEqual(cfg.ExcludeRepos, []string{"acme-inc/legacy", "other/repo"}) {
		t.Errorf("ExcludeRepos = %v", cfg.ExcludeRepos)
	}
	if len(cfg.Orgs) != 0 {
		t.Errorf("empty ORGS became %v", cfg.Orgs)
	}
	if cfg.MaxConcurrent != 3 || cfg.MaxRetries != 3 || cfg.PollInterval != 300 {
		t.Errorf("numbers: %+v", cfg)
	}
	if cfg.SkipBots {
		t.Error("SKIP_BOTS=0 did not turn bots back on")
	}
	if !cfg.SkipDrafts || !cfg.SkipOwn || !cfg.IncludeTeams || !cfg.Notify {
		t.Errorf("flags: %+v", cfg)
	}
	if cfg.reviewArgs != "--dry-run" {
		t.Errorf("ReviewArgs = %q", cfg.reviewArgs)
	}
	// Keys the old file never had must fall back to the defaults.
	if cfg.Reviewers != Default().Reviewers || cfg.Nice != Default().Nice ||
		cfg.AutoMergeTimeout != Default().AutoMergeTimeout {
		t.Errorf("new keys did not default: %+v", cfg)
	}
}

func TestLoadMissingFileIsDefault(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing file should not be an error: %v", err)
	}
	if !reflect.DeepEqual(cfg, Default()) {
		t.Error("missing file did not produce the defaults")
	}
}

func TestDefaultAutoMergeTimeoutCoversLongChecks(t *testing.T) {
	if got := Default().AutoMergeTimeout; got != 2*time.Hour {
		t.Fatalf("AutoMergeTimeout = %s, want 2h", got)
	}
}

func TestLoadReportsUnparsableLines(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	// Anything executable in the old shell config must be reported, not run.
	// Comments and blank lines are neither.
	body := "# a comment\n   \nORGS=\"acme\"\nsource /etc/passwd\nexport FOO=1\nMAX_CONCURRENT=8\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err == nil {
		t.Fatal("an unparsable line was accepted silently")
	}
	for _, want := range []string{"line 4: not an assignment: source /etc/passwd", "line 5: not an assignment: export FOO=1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not report %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "line 1") || strings.Contains(err.Error(), "line 2") {
		t.Errorf("a comment or blank line was reported: %v", err)
	}
	if cfg.MaxConcurrent != 8 {
		t.Errorf("MAX_CONCURRENT after the bad lines = %d, want 8", cfg.MaxConcurrent)
	}
	// The rest of the file still has to be usable.
	if !reflect.DeepEqual(cfg.Orgs, []string{"acme"}) {
		t.Errorf("Orgs = %v", cfg.Orgs)
	}
}

func TestBabysitDraftAndConflictDefaults(t *testing.T) {
	cfg := Default()
	if cfg.BabysitDrafts {
		t.Error("drafts default to allowed; babysitting a draft must be an explicit choice")
	}
	if !cfg.ResolveConflicts {
		t.Error("conflict resolution defaults to off; a conflicted branch would stall every run")
	}
	if !cfg.FixSuggestions {
		t.Error("the suggestion round defaults to off; leftover Suggestions would stay unaddressed")
	}
}

func TestRoundTrip(t *testing.T) {
	want := Default()
	want.Orgs = []string{"acme-inc", "myorg"}
	want.ExcludeRepos = []string{"acme-inc/legacy"}
	want.Teams = []string{"acme-inc/backend"}
	want.MaxConcurrent = 1
	want.Nice = 5
	want.LoadLimit = 12.5
	want.CacheBudgetGB = 2
	want.SkipOwn = false
	want.Notify = false
	want.NotifyReadyToMerge = true
	want.ReviewModel = "gpt-5.4-mini"
	want.ReviewEffort = "low"
	want.Post = false
	want.FixEffort = "high"
	want.MaxIter = 4
	want.FixTimeout = 90 * time.Minute
	want.DivergenceScan = true
	want.DivergenceEscalateTo = []string{"example-user", "acme/platform"}
	want.Sandboxed = true
	want.BabysitDrafts = true
	want.ResolveConflicts = false
	want.FixSuggestions = false
	want.AgentAction = ActionBabysit
	want.AutoMerge = true
	want.AutoMergeTimeout = 90 * time.Minute
	want.AutoMergeAuthors = []string{"example-user", "trusted-bot[bot]"}
	want.unknown = map[string]string{"FUTURE_KEY": "keep me"}

	path := filepath.Join(t.TempDir(), "config")
	if err := want.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round trip changed the config:\n got %+v\nwant %+v", got, want)
	}
	if got.unknown["FUTURE_KEY"] != "keep me" {
		t.Error("an unrecognised key was dropped on rewrite")
	}
}

// REVIEW_ARGS was passed verbatim to a separate binary that no longer exists.
// It has to keep working on read, because --dry-run through it is how people
// ran reviews without posting, and it has to disappear on write, because
// leaving it in a rewritten file would suggest it still does something.
func TestRetiredReviewArgsMapsDryRunAndIsNotRewritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("REVIEW_ARGS=\"--dry-run -n 4\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Post {
		t.Error("REVIEW_ARGS=--dry-run did not turn posting off")
	}
	if got.reviewArgs != "--dry-run -n 4" {
		t.Errorf("the old value was not preserved for the user to see: %q", got.reviewArgs)
	}

	if err := got.Save(path); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.reviewArgs != "" {
		t.Errorf("REVIEW_ARGS survived a rewrite as a live setting: %q", again.reviewArgs)
	}
	if again.Post {
		t.Error("POST was not written out, so the dry-run intent was lost on rewrite")
	}
}

// The three per-source auto-merge keys collapsed into one AUTO_MERGE. The old
// agent key migrates, the other two are read no more and vanish on rewrite.
func TestRetiredPerSourceAutoMergeKeysMigrateFromAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	legacy := "AUTO_MERGE_AGENT=1\nAUTO_MERGE_REVIEW=0\nAUTO_MERGE_BABYSIT=1\n"
	if err := os.WriteFile(path, []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.AutoMerge {
		t.Fatal("AUTO_MERGE_AGENT=1 did not migrate to AutoMerge")
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	rewritten, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"AUTO_MERGE_AGENT", "AUTO_MERGE_REVIEW", "AUTO_MERGE_BABYSIT"} {
		if strings.Contains(string(rewritten), key) {
			t.Errorf("rewritten config still contains %s", key)
		}
	}
	if !strings.Contains(string(rewritten), "AUTO_MERGE=1") {
		t.Error("rewritten config lost AUTO_MERGE=1")
	}
}

// Every review runs all of its passes at once, so the peak is simply the
// product. Nothing divides the passes between reviews any more.
func TestCodexPeakIsTheProduct(t *testing.T) {
	cases := []struct{ maxConcurrent, reviewers, want int }{
		{6, 6, 36},
		{10, 6, 60},
		{1, 6, 6},
		{3, 4, 12},
		{0, 6, 6}, // a nonsense setting still has to mean at least one review
	}
	for _, c := range cases {
		cfg := Default()
		cfg.MaxConcurrent, cfg.Reviewers = c.maxConcurrent, c.reviewers
		if got := cfg.Codex(); got != c.want {
			t.Errorf("MaxConcurrent=%d Reviewers=%d: got %d, want %d",
				c.maxConcurrent, c.reviewers, got, c.want)
		}
	}
}

// A config written by an older prbot still loads, and the retired key does not
// survive the next write.
func TestRetiredMaxCodexIsDropped(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config")
	if err := os.WriteFile(path, []byte("MAX_CONCURRENT=3\nMAX_CODEX=12\nREVIEWERS=6\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConcurrent != 3 || cfg.Reviewers != 6 {
		t.Errorf("cfg = %+v", cfg)
	}
	if _, ok := cfg.unknown["MAX_CODEX"]; ok {
		t.Error("the retired key was kept as an unknown assignment")
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), "MAX_CODEX") {
		t.Errorf("the retired key was written back:\n%s", b)
	}
}

// A config written before the effort levels became engine-specific can pair a
// codex level with the claude engine. That combination must not fail every
// run: the launchd agent has no terminal to report it in, so the symptom
// would be an agent that silently stops reviewing.
func TestLoadDropsAnEffortTheEngineCannotUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	body := `REVIEW_ENGINE="claude"
REVIEW_EFFORT="ultra"
FIX_ENGINE="codex"
FIX_EFFORT="ultra"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReviewEffort != "" {
		t.Errorf("ReviewEffort = %q, want it dropped for the claude engine", cfg.ReviewEffort)
	}
	if cfg.FixEffort != "ultra" {
		t.Errorf("FixEffort = %q, want the codex level kept", cfg.FixEffort)
	}
}

// LOOP_MODE gates whether babysit pushes once at the end (offline, the
// default) or after every round. An unknown value keeps the default rather
// than failing behind the launchd agent, matching AGENT_ACTION.
func TestLoopModeParsesAndSurvivesARewrite(t *testing.T) {
	if got := Default().LoopMode; got != LoopOffline {
		t.Fatalf("default LoopMode = %q, want %q", got, LoopOffline)
	}
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("LOOP_MODE=\"online\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoopMode != LoopOnline {
		t.Fatalf("LoopMode = %q, want %q", cfg.LoopMode, LoopOnline)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.LoopMode != LoopOnline {
		t.Fatalf("LoopMode after rewrite = %q, want %q", again.LoopMode, LoopOnline)
	}

	if err := os.WriteFile(path, []byte("LOOP_MODE=\"sideways\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err = Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LoopMode != LoopOffline {
		t.Fatalf("an unknown LOOP_MODE changed the default: %q", cfg.LoopMode)
	}
}

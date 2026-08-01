package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestUnquote(t *testing.T) {
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
	for _, c := range cases {
		if got := unquote(c.in); got != c.want {
			t.Errorf("unquote(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestParseLine(t *testing.T) {
	if _, _, ok := parseLine("# just a comment"); ok {
		t.Error("comment parsed as assignment")
	}
	if _, _, ok := parseLine("   "); ok {
		t.Error("blank line parsed as assignment")
	}
	// Anything executable in the old shell config must be reported, not run.
	msg, _, ok := parseLine("rm -rf /")
	if ok {
		t.Error("a command parsed as an assignment")
	}
	if !strings.Contains(msg, "not an assignment") {
		t.Errorf("unhelpful complaint: %q", msg)
	}
	if _, _, ok := parseLine("export FOO=1"); ok {
		t.Error("export line parsed as a plain assignment")
	}
	k, v, ok := parseLine("MAX_CODEX=8")
	if !ok || k != "MAX_CODEX" || v != "8" {
		t.Errorf("got %q=%q ok=%v", k, v, ok)
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
	if cfg.ReviewArgs != "--dry-run" {
		t.Errorf("ReviewArgs = %q", cfg.ReviewArgs)
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
	if err := os.WriteFile(path, []byte("ORGS=\"acme\"\nsource /etc/passwd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err == nil {
		t.Fatal("an unparsable line was accepted silently")
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
	want.AgentAction = ActionBabysit
	want.AutoMergeAgent = true
	want.AutoMergeReview = true
	want.AutoMergeBabysit = true
	want.AutoMergeTimeout = 90 * time.Minute
	want.Unknown = map[string]string{"FUTURE_KEY": "keep me"}

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
	if got.Unknown["FUTURE_KEY"] != "keep me" {
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
	if got.ReviewArgs != "--dry-run -n 4" {
		t.Errorf("the old value was not preserved for the user to see: %q", got.ReviewArgs)
	}

	if err := got.Save(path); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if again.ReviewArgs != "" {
		t.Errorf("REVIEW_ARGS survived a rewrite as a live setting: %q", again.ReviewArgs)
	}
	if again.Post {
		t.Error("POST was not written out, so the dry-run intent was lost on rewrite")
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
	if _, ok := cfg.Unknown["MAX_CODEX"]; ok {
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

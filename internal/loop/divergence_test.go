package loop

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/envexec"
	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/git"
	"github.com/yungweng/quorum/internal/review"
	"github.com/yungweng/quorum/internal/target"
)

func sampleDivergenceTrace() DivergenceTrace {
	return DivergenceTrace{
		Schema: 1, PR: 42, Title: "Choose a retry policy", Description: "Keep retries bounded.",
		InitialHead: "aaaaaaaa", FinalHead: "cccccccc",
		Rounds: []DivergenceRoundTrace{
			{Round: 1, ReviewedSHA: "aaaaaaaa", ReviewComment: "Preserve the request.",
				Critical: 1, Fix: &FixTrace{BeforeSHA: "aaaaaaaa", AfterSHA: "bbbbbbbb"}},
			{Round: 2, ReviewedSHA: "bbbbbbbb", ReviewComment: "Cancel the request.",
				Critical: 1, Fix: &FixTrace{BeforeSHA: "bbbbbbbb", AfterSHA: "cccccccc"}},
		},
	}
}

func TestDivergenceTimeoutSurvivesDefaults(t *testing.T) {
	for _, timeout := range []time.Duration{0, 12 * time.Minute} {
		got := (Options{DivergenceTimeout: timeout}).withDefaults().DivergenceTimeout
		if got != timeout {
			t.Errorf("divergence timeout = %s, want %s", got, timeout)
		}
	}
}

func TestValidateDivergenceReportRequiresConcreteEvidence(t *testing.T) {
	trace := sampleDivergenceTrace()
	report := DivergenceReport{
		Schema: 1, Verdict: DivergenceDiverged, Summary: "The policy alternates.",
		Recommendation: "Choose one ownership rule.",
		Conflicts: []DivergenceConflict{{
			Title: "Request ownership", Scope: "merge policy",
			DecisionA: "Preserve requests", DecisionB: "Cancel requests",
			EvidenceA: []DivergenceEvidence{{Round: 1, SHA: "aaaaaaaa", Summary: "Round 1 preserves it."}},
			EvidenceB: []DivergenceEvidence{{Round: 2, SHA: "bbbbbbbb", Summary: "Round 2 cancels it."}},
		}},
	}
	if err := validateDivergenceReport(report, trace); err != nil {
		t.Fatalf("valid report failed: %v", err)
	}

	report.Conflicts[0].EvidenceB[0].SHA = "unknown"
	if err := validateDivergenceReport(report, trace); err == nil {
		t.Fatal("report cited a SHA outside the trace")
	}
	report.Conflicts = nil
	if err := validateDivergenceReport(report, trace); err == nil {
		t.Fatal("diverged report without conflicts was accepted")
	}
}

func TestValidateNonDivergedReportRejectsInventedConflicts(t *testing.T) {
	report := DivergenceReport{
		Schema: 1, Verdict: DivergenceCumulative, Summary: "Findings are compatible.",
		Recommendation: "Review the remaining failures.",
		Conflicts:      []DivergenceConflict{{Title: "invented"}},
	}
	if err := validateDivergenceReport(report, sampleDivergenceTrace()); err == nil {
		t.Fatal("cumulative report with conflicts was accepted")
	}
	report.Conflicts = nil
	if err := validateDivergenceReport(report, sampleDivergenceTrace()); err != nil {
		t.Fatalf("cumulative report failed: %v", err)
	}
}

func TestDivergenceReportControlsMentions(t *testing.T) {
	got := divergenceMentions("example-user", []string{"example-user", "acme/platform", "second-user"})
	want := []string{"@example-user", "@acme/platform", "@second-user"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("mentions = %v, want %v", got, want)
	}
	for _, bad := range []string{"@already-prefixed", "bad target", "org/"} {
		if validDivergenceTarget(bad) {
			t.Errorf("validDivergenceTarget(%q) = true", bad)
		}
	}

	report := DivergenceReport{
		Schema: 1, Verdict: DivergenceCumulative, Summary: "The findings are cumulative.",
		Recommendation: "Inspect the latest fix.",
	}
	body := renderDivergenceReport(report, 12, nil)
	if !strings.Contains(body, "after 12 rounds") || strings.Contains(body, "@") {
		t.Fatalf("unexpected report body:\n%s", body)
	}
}

func TestAnalyzeDivergenceUsesStrictJSON(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	result := `{"schema":1,"verdict":"cumulative","summary":"Findings are compatible.","conflicts":[],"recommendation":"Inspect the latest fix."}`
	script := "#!/bin/sh\n" +
		"out=''\n" +
		"while test $# -gt 0; do\n" +
		"  if test \"$1\" = -o; then shift; out=$1; fi\n" +
		"  shift\n" +
		"done\n" +
		"printf '%s' '" + result + "' > \"$out\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &run{
		o:    Options{CodexBin: bin},
		ctx:  context.Background(),
		root: dir, logDir: dir,
		env:             envexec.Env{Worktree: dir},
		divergenceTrace: sampleDivergenceTrace(),
	}
	report, err := r.analyzeDivergence()
	if err != nil {
		t.Fatal(err)
	}
	if report.Verdict != DivergenceCumulative {
		t.Fatalf("verdict = %q", report.Verdict)
	}
	script = strings.Replace(script, result, result+" trailing", 1)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := r.analyzeDivergence(); err == nil || !strings.Contains(err.Error(), "trailing content") {
		t.Fatalf("trailing output error = %v", err)
	}
}

func TestAnalyzeDivergenceHonorsTimeout(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &run{
		o:    Options{CodexBin: bin, DivergenceTimeout: 10 * time.Millisecond},
		ctx:  context.Background(),
		root: dir, logDir: dir,
		env:             envexec.Env{Worktree: dir},
		divergenceTrace: sampleDivergenceTrace(),
	}
	if _, err := r.analyzeDivergence(); err == nil || !strings.Contains(err.Error(), "analysis failed") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestAnalyzeDivergenceHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nsleep 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &run{
		o:    Options{CodexBin: bin, DivergenceTimeout: time.Minute},
		ctx:  ctx,
		root: dir, logDir: dir,
		env:             envexec.Env{Worktree: dir},
		divergenceTrace: sampleDivergenceTrace(),
	}
	if _, err := r.analyzeDivergence(); err == nil || !strings.Contains(err.Error(), "analysis failed") {
		t.Fatalf("cancellation error = %v", err)
	}
}

func TestRunDivergenceScanWritesLocalArtifactsWithoutPosting(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "codex")
	result := `{"schema":1,"verdict":"uncertain","summary":"The evidence is incomplete.","conflicts":[],"recommendation":"Inspect both policies manually."}`
	script := "#!/bin/sh\n" +
		"out=''\n" +
		"while test $# -gt 0; do\n" +
		"  if test \"$1\" = -o; then shift; out=$1; fi\n" +
		"  shift\n" +
		"done\n" +
		"printf '%s' '" + result + "' > \"$out\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &run{
		p:               &Pipeline{Git: git.New("git")},
		o:               Options{CodexBin: bin, DivergenceScan: true, Post: false},
		ctx:             context.Background(),
		rep:             NopReporter{},
		root:            dir,
		logDir:          dir,
		env:             envexec.Env{Worktree: dir},
		target:          target.Target{BranchOnly: true},
		divergenceTrace: sampleDivergenceTrace(),
	}
	res := &Result{}
	if err := r.runDivergenceScan(res); err != nil {
		t.Fatal(err)
	}
	if res.Divergence == nil || res.Divergence.Verdict != DivergenceUncertain {
		t.Fatalf("result = %+v", res.Divergence)
	}
	for _, name := range []string{DivergenceResultFile, DivergenceReportFile, "divergence.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s was not written: %v", name, err)
		}
	}
}

func TestTraceMarksTheFinalFixAsUnreviewed(t *testing.T) {
	dir := t.TempDir()
	gitBin := filepath.Join(dir, "git")
	if err := os.WriteFile(gitBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &run{
		p: &Pipeline{Git: git.New(gitBin)}, o: Options{DivergenceScan: true},
		ctx: context.Background(), root: dir, worktree: dir,
		divergenceTrace: DivergenceTrace{Schema: 1, InitialHead: "old", FinalHead: "old"},
	}
	r.traceReview(1, review.Findings{HeadSHA: "old", Critical: 1}, "Finding")
	r.lastMsg = "Fixed it."
	r.traceFix(1, "old", "new", "fix-round-1")
	if got := r.divergenceTrace.Rounds[0].Fix.AfterSHA; got != "new" {
		t.Fatalf("final fix = %q", got)
	}
	if r.divergenceTrace.FinalHead != "new" || r.divergenceTrace.Rounds[0].ReviewedSHA != "old" {
		t.Fatalf("trace did not distinguish reviewed and final heads: %+v", r.divergenceTrace)
	}
}

func TestTraceRecordsCIFixAndAdvancesFinalHead(t *testing.T) {
	dir := t.TempDir()
	gitBin := filepath.Join(dir, "git")
	if err := os.WriteFile(gitBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &run{
		p: &Pipeline{Git: git.New(gitBin)}, o: Options{DivergenceScan: true},
		ctx: context.Background(), root: dir, worktree: dir,
		divergenceTrace: DivergenceTrace{Schema: 1, InitialHead: "old", FinalHead: "old"},
	}
	r.lastMsg = "Repaired the failing check."
	r.traceCIFix("old", "new", "ci-fix-1")

	if len(r.divergenceTrace.CIFixes) != 1 {
		t.Fatalf("CI fixes = %d, want 1", len(r.divergenceTrace.CIFixes))
	}
	fix := r.divergenceTrace.CIFixes[0]
	if fix.BeforeSHA != "old" || fix.AfterSHA != "new" || fix.Response != r.lastMsg {
		t.Fatalf("CI fix = %+v", fix)
	}
	if r.divergenceTrace.FinalHead != "new" {
		t.Fatalf("final head = %q, want new", r.divergenceTrace.FinalHead)
	}
}

func TestDisabledDivergenceScanHasNoArtifactsOrValidation(t *testing.T) {
	dir := t.TempDir()
	r := &run{o: Options{DivergenceScan: false}, root: dir}
	r.divergenceTrace = sampleDivergenceTrace()
	r.writeDivergenceTrace()
	if _, err := os.Stat(filepath.Join(dir, DivergenceTraceFile)); !os.IsNotExist(err) {
		t.Fatalf("disabled scan wrote an artifact: %v", err)
	}
	if err := (Options{
		Repo: "acme/api", RepoRoot: dir, MaxIter: 1,
		DivergenceEscalateTo: []string{"bad target"},
	}).validate(); err != nil {
		t.Fatalf("disabled scan rejected dormant config: %v", err)
	}
	if err := (Options{
		Repo: "acme/api", RepoRoot: dir, MaxIter: 1, DivergenceScan: true,
		DivergenceEscalateTo: []string{"bad target"},
	}).validate(); err == nil {
		t.Fatal("enabled scan accepted an invalid escalation target")
	}
}

func TestDivergenceReportIsNotPostedAfterHeadDrift(t *testing.T) {
	dir := t.TempDir()
	codexBin := filepath.Join(dir, "codex")
	result := `{"schema":1,"verdict":"cumulative","summary":"Findings are compatible.","conflicts":[],"recommendation":"Inspect the latest fix."}`
	codexScript := "#!/bin/sh\n" +
		"out=''\n" +
		"while test $# -gt 0; do\n" +
		"  if test \"$1\" = -o; then shift; out=$1; fi\n" +
		"  shift\n" +
		"done\n" +
		"printf '%s' '" + result + "' > \"$out\"\n"
	if err := os.WriteFile(codexBin, []byte(codexScript), 0o755); err != nil {
		t.Fatal(err)
	}
	commented := filepath.Join(dir, "commented")
	ghBin := filepath.Join(dir, "gh")
	ghScript := "#!/bin/sh\n" +
		"if test \"$1\" = pr && test \"$2\" = view; then printf '%s' moved-head; exit 0; fi\n" +
		"if test \"$1\" = pr && test \"$2\" = comment; then touch '" + commented + "'; fi\n"
	if err := os.WriteFile(ghBin, []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	rep := &warningReporter{}
	r := &run{
		p:               &Pipeline{GH: gh.New(ghBin), Git: git.New("git")},
		o:               Options{CodexBin: codexBin, DivergenceScan: true, Post: true, RepoRoot: dir},
		ctx:             context.Background(),
		rep:             rep,
		root:            dir,
		logDir:          dir,
		env:             envexec.Env{Worktree: dir},
		pr:              gh.FullPR{Number: 42},
		divergenceTrace: sampleDivergenceTrace(),
	}
	if err := r.runDivergenceScan(&Result{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(commented); !os.IsNotExist(err) {
		t.Fatalf("head-drifted report was posted: %v", err)
	}
	if len(rep.warnings) != 1 || !strings.Contains(rep.warnings[0], "not posting") {
		t.Fatalf("warnings = %v", rep.warnings)
	}
}

func TestUncertainDivergenceReportPostsConfiguredEscalation(t *testing.T) {
	dir := t.TempDir()
	codexBin := filepath.Join(dir, "codex")
	result := `{"schema":1,"verdict":"uncertain","summary":"The evidence is incomplete.","conflicts":[],"recommendation":"Choose the intended policy."}`
	codexScript := "#!/bin/sh\n" +
		"out=''\n" +
		"while test $# -gt 0; do\n" +
		"  if test \"$1\" = -o; then shift; out=$1; fi\n" +
		"  shift\n" +
		"done\n" +
		"printf '%s' '" + result + "' > \"$out\"\n"
	if err := os.WriteFile(codexBin, []byte(codexScript), 0o755); err != nil {
		t.Fatal(err)
	}
	commentBody := filepath.Join(dir, "comment-body")
	ghBin := filepath.Join(dir, "gh")
	ghScript := "#!/bin/sh\n" +
		"if test \"$1\" = pr && test \"$2\" = view; then printf '%s' cccccccc; exit 0; fi\n" +
		"if test \"$1\" = pr && test \"$2\" = comment; then printf '%s' \"$5\" > '" + commentBody + "'; printf '%s' https://example.invalid/report; fi\n"
	if err := os.WriteFile(ghBin, []byte(ghScript), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &run{
		p: &Pipeline{GH: gh.New(ghBin), Git: git.New("git")},
		o: Options{
			CodexBin: codexBin, DivergenceScan: true, Post: true, RepoRoot: dir,
			DivergenceEscalateTo: []string{"acme/platform"},
		},
		ctx: context.Background(), rep: NopReporter{},
		root: dir, logDir: dir, env: envexec.Env{Worktree: dir},
		pr: gh.FullPR{Number: 42, Author: struct {
			Login string `json:"login"`
		}{Login: "example-user"}},
		divergenceTrace: sampleDivergenceTrace(),
	}
	res := &Result{}
	if err := r.runDivergenceScan(res); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(commentBody)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"@example-user", "@acme/platform", "**Verdict:** uncertain"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("comment is missing %q:\n%s", want, body)
		}
	}
	if res.DivergenceCommentURL != "https://example.invalid/report" {
		t.Fatalf("comment URL = %q", res.DivergenceCommentURL)
	}
}

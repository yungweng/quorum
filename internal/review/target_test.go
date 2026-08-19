package review

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/gh"
	"github.com/yungweng/quorum/internal/target"
)

func TestResumeKeepsTheSavedBranchTargetAfterAPRIsOpened(t *testing.T) {
	root := t.TempDir()
	meta := runTarget{
		Schema:     runTargetSchema,
		Repo:       "acme/api",
		Branch:     "feature/crumb-tray",
		BaseBranch: "main",
		BranchOnly: true,
	}
	if err := writeRunTarget(filepath.Join(root, "target.json"), meta); err != nil {
		t.Fatal(err)
	}

	ghBin := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(ghBin, []byte(`#!/bin/sh
echo "GitHub must not be queried for a saved branch-only target" >&2
exit 1
`), 0o755); err != nil {
		t.Fatal(err)
	}
	o := Options{
		Repo:      "acme/api",
		RepoRoot:  t.TempDir(),
		ResumeRun: root,
	}
	r := &Runner{GH: gh.New(ghBin), Git: fakeReviewGit(t)}

	tgt, run, err := r.resolveRunTarget(context.Background(), &o)
	if err != nil {
		t.Fatal(err)
	}
	if !tgt.BranchOnly || tgt.PR.Number != 0 ||
		tgt.PR.HeadRefName != meta.Branch || tgt.PR.BaseRefName != meta.BaseBranch {
		t.Fatalf("resolved target = %+v", tgt)
	}
	if o.Branch != meta.Branch || o.BaseBranch != meta.BaseBranch || run.root != root {
		t.Fatalf("resume options/run = %+v, %q", o, run.root)
	}
}

func TestResumeRefusesTargetOverrides(t *testing.T) {
	meta := runTarget{
		Schema:     runTargetSchema,
		Repo:       "acme/api",
		Branch:     "feature/crumb-tray",
		BaseBranch: "main",
		BranchOnly: true,
	}
	for name, o := range map[string]Options{
		"repository": {Repo: "other/api"},
		"PR":         {Repo: "acme/api", Number: 42},
		"branch":     {Repo: "acme/api", Branch: "feature/other"},
		"base":       {Repo: "acme/api", BaseBranch: "develop"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := applyRunTarget(&o, meta); err == nil {
				t.Fatalf("override was accepted: %+v", o)
			}
		})
	}
}

func TestLegacyResumeOnlyAcceptsItsGeneratedPRName(t *testing.T) {
	pr := targetForTest(42, false)
	for _, test := range []struct {
		name string
		root string
		tgt  target.Target
		want bool
	}{
		{
			name: "matching PR",
			root: "/tmp/acme-api-pr-42-20260730-220000",
			tgt:  pr,
			want: true,
		},
		{
			name: "different PR",
			root: "/tmp/acme-api-pr-41-20260730-220000",
			tgt:  pr,
		},
		{
			name: "branch",
			root: "/tmp/acme-api-branch-feature-crumb-tray-20260730-220000",
			tgt:  targetForTest(0, true),
		},
		{
			name: "branch name resembles PR target",
			root: "/tmp/acme-api-branch-pr-42-20260730-220000",
			tgt: target.Target{PR: gh.FullPR{
				Number:      42,
				HeadRefName: "pr-42",
			}},
		},
		{
			name: "renamed directory",
			root: "/tmp/recovered-review",
			tgt:  pr,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := legacyPRRunMatches(test.root, test.tgt); got != test.want {
				t.Fatalf("legacyPRRunMatches() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestRunTargetRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "target.json")
	pr := gh.FullPR{Number: 42}
	want := runTarget{
		Schema:     runTargetSchema,
		Repo:       "acme/api",
		Number:     42,
		Branch:     "feature/crumb-tray",
		BaseBranch: "main",
		PR:         &pr,
	}
	if err := writeRunTarget(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := readRunTarget(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("metadata = %+v, want %+v", got, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"base_branch": "main"`) {
		t.Fatalf("metadata did not persist the base:\n%s", data)
	}
}

func targetForTest(number int, branchOnly bool) target.Target {
	return target.Target{
		BranchOnly: branchOnly,
		PR: gh.FullPR{
			Number:      number,
			HeadRefName: "feature/crumb-tray",
			BaseRefName: "main",
		},
	}
}

// A local-head run reviews a commit only the caller's worktree has. The
// metadata must pin that exact commit so a resume reviews it again instead of
// re-resolving the branch from origin, which never had it - and an online
// run's directory must never satisfy a local-head resume.
func TestRunTargetMetadataPinsALocalHead(t *testing.T) {
	tgt := targetForTest(0, true)
	meta := newRunTarget(Options{Repo: "acme/api", LocalHead: true, HeadSHA: "abc123"}, tgt, "main")
	if meta.LocalHead != "abc123" {
		t.Fatalf("LocalHead = %q, want the pinned sha", meta.LocalHead)
	}
	if err := meta.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	o := Options{Repo: "acme/api", LocalHead: true}
	if err := applyRunTarget(&o, meta); err != nil {
		t.Fatalf("applyRunTarget: %v", err)
	}
	if !o.LocalHead || o.HeadSHA != "abc123" {
		t.Fatalf("resume did not restore the local head: %+v", o)
	}

	other := Options{Repo: "acme/api", LocalHead: true, HeadSHA: "def456"}
	if err := applyRunTarget(&other, meta); err == nil {
		t.Fatal("a different pinned sha was accepted")
	}

	online := newRunTarget(Options{Repo: "acme/api"}, tgt, "main")
	fromOnline := Options{Repo: "acme/api", LocalHead: true}
	if err := applyRunTarget(&fromOnline, online); err == nil {
		t.Fatal("an online run's directory satisfied a local-head resume")
	}

	pr := gh.FullPR{Number: 42, Title: "Keep report context", URL: "https://example.invalid/pr/42"}
	pr.Author.Login = "example-user"
	prMeta := runTarget{Schema: runTargetSchema, Repo: "acme/api", Number: 42,
		Branch: "feature/crumb-tray", BaseBranch: "main", LocalHead: "abc123", PR: &pr}
	if err := prMeta.validate(); err != nil {
		t.Fatalf("a local PR head did not validate: %v", err)
	}
	prResume := Options{Repo: "acme/api", RepoRoot: t.TempDir(), Number: 42, LocalHead: true}
	if err := applyRunTarget(&prResume, prMeta); err != nil {
		t.Fatalf("a local PR head did not resume: %v", err)
	}
	tgt, _, err := (&Runner{}).resolveRunTarget(context.Background(), &prResume)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.BranchOnly || tgt.PR.Number != pr.Number || tgt.PR.Title != pr.Title || tgt.PR.URL != pr.URL || tgt.PR.Author.Login != pr.Author.Login {
		t.Fatalf("resume lost PR metadata: %+v", tgt)
	}
}

func TestResolveLocalHeadKeepsProvidedPRMetadata(t *testing.T) {
	pr := gh.FullPR{Number: 42, Title: "Keep report context", URL: "https://example.invalid/pr/42"}
	pr.Author.Login = "example-user"
	o := Options{
		Repo: "acme/api", RepoRoot: t.TempDir(), LocalHead: true, LocalPR: &pr,
		Branch: "feature/crumb-tray", HeadSHA: "abc123", BaseBranch: "main",
	}.withDefaults()
	tgt, _, err := (&Runner{}).resolveRunTarget(context.Background(), &o)
	if err != nil {
		t.Fatal(err)
	}
	if tgt.BranchOnly || tgt.PR.Number != 42 || tgt.PR.Title != pr.Title || tgt.PR.URL != pr.URL || tgt.PR.Author.Login != pr.Author.Login {
		t.Fatalf("local target = %+v", tgt)
	}
	if tgt.PR.HeadRefName != o.Branch || tgt.PR.HeadRefOid != o.HeadSHA || tgt.PR.BaseRefName != o.BaseBranch {
		t.Fatalf("local target did not pin the supplied refs: %+v", tgt.PR)
	}
}

// LocalHead without the branch, sha and base is a programming error in the
// caller, and reviewing origin's older head instead would be silent and wrong.
func TestValidateRequiresTheLocalHeadFields(t *testing.T) {
	o := Options{Repo: "acme/api", RepoRoot: "/tmp/x", LocalHead: true}.withDefaults()
	if err := o.validate(); err == nil {
		t.Fatal("an incomplete local-head target validated")
	}
	o.Branch, o.HeadSHA, o.BaseBranch = "feature/crumb-tray", "abc123", "main"
	if err := o.validate(); err != nil {
		t.Fatalf("a complete local-head target failed: %v", err)
	}
}

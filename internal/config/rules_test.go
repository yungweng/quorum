package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepoRulesMissingFileMeansNoRules(t *testing.T) {
	got, err := RepoRules(t.TempDir(), "acme/api")
	if err != nil || got != "" {
		t.Fatalf("RepoRules = %q, %v; want empty and no error", got, err)
	}
}

func TestRepoRulesReadsTheOwnerRepoFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	want := "- No new UI components; reuse existing ones first (Blocker)."
	if err := os.WriteFile(filepath.Join(dir, "acme", "api.md"), []byte(want+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := RepoRules(dir, "acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("RepoRules = %q, want %q", got, want)
	}
}

func TestRepoRulesRefusesPathTraversal(t *testing.T) {
	got, err := RepoRules(t.TempDir(), "../outside/escape")
	if err != nil || got != "" {
		t.Fatalf("RepoRules = %q, %v; want empty and no error", got, err)
	}
}

func TestRepoTestCmdReadsTheOwnerRepoFile(t *testing.T) {
	dir := t.TempDir()
	if got, set, err := RepoTestCmd(dir, "acme/api"); err != nil || set || got != "" {
		t.Fatalf("RepoTestCmd = %q, %v, %v; want empty, false, and no error for a missing file", got, set, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "acme", "api")
	if err := os.WriteFile(path, []byte("make check\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, set, err := RepoTestCmd(dir, "acme/api")
	if err != nil || !set {
		t.Fatal(err)
	}
	if got != "make check" {
		t.Errorf("RepoTestCmd = %q, want %q", got, "make check")
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if got, set, err := RepoTestCmd(dir, "acme/api"); err != nil || !set || got != "" {
		t.Fatalf("RepoTestCmd empty override = %q, %v, %v; want empty, true, nil", got, set, err)
	}
	if got, set, err := RepoTestCmd(dir, "../outside/escape"); err != nil || set || got != "" {
		t.Fatalf("RepoTestCmd = %q, %v, %v; want traversal refused silently", got, set, err)
	}
}

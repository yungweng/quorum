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
	if got, err := RepoTestCmd(dir, "acme/api"); err != nil || got != "" {
		t.Fatalf("RepoTestCmd = %q, %v; want empty and no error for a missing file", got, err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "acme"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "acme", "api"), []byte("make check\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := RepoTestCmd(dir, "acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if got != "make check" {
		t.Errorf("RepoTestCmd = %q, want %q", got, "make check")
	}
	if got, err := RepoTestCmd(dir, "../outside/escape"); err != nil || got != "" {
		t.Fatalf("RepoTestCmd = %q, %v; want traversal refused silently", got, err)
	}
}

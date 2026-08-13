package notify

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestApprovalRequiredCommandUsesDedicatedPersistentGroup(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "terminal-notifier")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	cmd, err := persistentCommand(
		"quorum: approval required",
		"acme/api#42 is clean. Ask another reviewer to approve it.",
		"https://github.com/acme/api/pull/42",
		approvalGroup("acme/api", 42),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		bin,
		"-title", "quorum: approval required",
		"-message", "acme/api#42 is clean. Ask another reviewer to approve it.",
		"-group", "io.github.quorum.approval.acme/api#42",
		"-sound", "default",
		"-open", "https://github.com/acme/api/pull/42",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("args = %q, want %q", cmd.Args, want)
	}
}

func TestReadyToMergeCommandUsesDedicatedPersistentGroup(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "terminal-notifier")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	cmd, err := persistentCommand(
		"quorum: ready to merge",
		"acme/api#42 is clean and ready to merge.",
		"https://github.com/acme/api/pull/42",
		readyGroup("acme/api", 42),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		bin,
		"-title", "quorum: ready to merge",
		"-message", "acme/api#42 is clean and ready to merge.",
		"-group", "io.github.quorum.ready.acme/api#42",
		"-sound", "default",
		"-open", "https://github.com/acme/api/pull/42",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("args = %q, want %q", cmd.Args, want)
	}
}

func TestReadyGroupDiffersFromApprovalGroup(t *testing.T) {
	if readyGroup("acme/api", 42) == approvalGroup("acme/api", 42) {
		t.Fatal("ready and approval notifications share a group, so one would replace the other")
	}
}

func TestPersistentCommandNeedsTerminalNotifier(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	cmd, err := persistentCommand("title", "body", "", "group")
	if err == nil || cmd != nil {
		t.Fatalf("persistentCommand = (%v, %v), want missing-tool error", cmd, err)
	}
	if !strings.Contains(err.Error(), "terminal-notifier not found") {
		t.Fatalf("error = %q", err)
	}
}

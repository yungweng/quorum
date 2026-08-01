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

	cmd, err := approvalRequiredCommand(
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

func TestApprovalRequiredCommandNeedsTerminalNotifier(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	cmd, err := approvalRequiredCommand("title", "body", "", "group")
	if err == nil || cmd != nil {
		t.Fatalf("approvalRequiredCommand = (%v, %v), want missing-tool error", cmd, err)
	}
	if !strings.Contains(err.Error(), "terminal-notifier not found") {
		t.Fatalf("error = %q", err)
	}
}

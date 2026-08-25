package notify

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestImportantNotificationUsesShortLivedClickableCommand(t *testing.T) {
	n, err := importantNotification(kindReady, "acme/api", 42, "https://github.com/acme/api/pull/42")
	if err != nil {
		t.Fatal(err)
	}
	cmd := terminalNotifierCommand("/opt/homebrew/bin/terminal-notifier", n)
	want := []string{
		"/opt/homebrew/bin/terminal-notifier",
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

func TestApprovalAndReadyNotificationsUseDifferentGroups(t *testing.T) {
	approval, err := importantNotification(kindApproval, "acme/api", 42, "https://example.invalid/42")
	if err != nil {
		t.Fatal(err)
	}
	ready, err := importantNotification(kindReady, "acme/api", 42, "https://example.invalid/42")
	if err != nil {
		t.Fatal(err)
	}
	if approval.group == ready.group {
		t.Fatal("ready and approval notifications share a group, so one would replace the other")
	}
}

func TestImportantDeliveryUsesClickableNotifier(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	notifier := writeScript(t, dir, "terminal-notifier", `printf '%s\n' "$@" > "$NOTIFIER_ARGS"`)
	t.Setenv("NOTIFIER_ARGS", argsFile)
	n, err := importantNotification(kindReady, "acme/api", 42, "https://github.com/acme/api/pull/42")
	if err != nil {
		t.Fatal(err)
	}
	if err := deliver(n, notifier); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "-open\n"+n.url) {
		t.Fatalf("notification is not clickable:\n%s", got)
	}
}

func TestImportantNotificationRejectsUnknownKind(t *testing.T) {
	if _, err := importantNotification("surprise", "acme/api", 42, "https://example.invalid/42"); err == nil {
		t.Fatal("unknown notification kind was accepted")
	}
}

func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

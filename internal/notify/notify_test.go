package notify

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestAlerterCommandKeepsImportantNotificationUntilDismissed(t *testing.T) {
	n, err := importantNotification(
		kindReady,
		"acme/api",
		42,
		"https://github.com/acme/api/pull/42",
	)
	if err != nil {
		t.Fatal(err)
	}

	cmd := alerterCommand("/opt/homebrew/bin/alerter", n)
	want := []string{
		"/opt/homebrew/bin/alerter",
		"--title", "quorum: ready to merge",
		"--message", "acme/api#42 is clean and ready to merge.",
		"--group", "io.github.quorum.ready.acme/api#42",
		"--sender", alerterBundleID,
		"--sound", "default",
		"--actions", openAction,
		"--close-label", dismissAction,
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("args = %q, want %q", cmd.Args, want)
	}
	for _, arg := range cmd.Args {
		if arg == "--timeout" {
			t.Fatal("persistent alert has a timeout")
		}
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

func TestImportantChildIsDetachedAndCarriesOnlyTypedArguments(t *testing.T) {
	n, err := importantNotification(kindReady, "acme/api", 42, "https://github.com/acme/api/pull/42")
	if err != nil {
		t.Fatal(err)
	}
	cmd := childCommand("/usr/local/bin/quorum", n)
	want := []string{
		"/usr/local/bin/quorum", "_notify", kindReady, "acme/api", "42",
		"https://github.com/acme/api/pull/42",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("child args = %q, want %q", cmd.Args, want)
	}
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setsid {
		t.Fatal("important notification child is not detached")
	}
}

func TestActivationOpensOnlyForContentOrOpenAction(t *testing.T) {
	for _, result := range []string{"@CONTENTCLICKED", "@ACTIONCLICKED", openAction} {
		if !requestsOpen(result) {
			t.Errorf("requestsOpen(%q) = false", result)
		}
	}
	for _, result := range []string{"", "@CLOSED", dismissAction, "@TIMEOUT"} {
		if requestsOpen(result) {
			t.Errorf("requestsOpen(%q) = true", result)
		}
	}
}

func TestDeliverOpensTheExactPRURL(t *testing.T) {
	dir := t.TempDir()
	delivered := filepath.Join(dir, "delivered")
	opened := filepath.Join(dir, "opened")
	alerter := writeScript(t, dir, "alerter", `
case "$1" in
  --version) echo 'alerter 26.5' ;;
  --list) test -f "$ALERTER_DELIVERED" && echo '[{"groupID":"ready"}]' ;;
  *) : > "$ALERTER_DELIVERED"; echo '@CONTENTCLICKED' ;;
esac
`)
	opener := writeScript(t, dir, "open", `printf '%s' "$1" > "$ALERTER_OPENED"`)
	t.Setenv("ALERTER_DELIVERED", delivered)
	t.Setenv("ALERTER_OPENED", opened)

	n, err := importantNotification(kindReady, "acme/api", 42, "https://github.com/acme/api/pull/42")
	if err != nil {
		t.Fatal(err)
	}
	if err := deliver(n, notifierTools{alerter: alerter, opener: opener}, nil, time.Second); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(opened)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != n.url {
		t.Fatalf("opened %q, want %q", got, n.url)
	}
}

func TestMissingAlerterSendsTemporaryClickableFallback(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "terminal-args")
	terminalNotifier := writeScript(t, dir, "terminal-notifier", `printf '%s\n' "$@" > "$TERMINAL_ARGS"`)
	t.Setenv("TERMINAL_ARGS", argsFile)
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	n, err := importantNotification(kindReady, "acme/api", 42, "https://github.com/acme/api/pull/42")
	if err != nil {
		t.Fatal(err)
	}
	if err := deliver(n, notifierTools{terminalNotifier: terminalNotifier}, logf, time.Second); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"-group\nio.github.quorum.ready.acme/api#42",
		"-open\nhttps://github.com/acme/api/pull/42",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("temporary fallback missing %q:\n%s", want, got)
		}
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "sent temporary fallback") {
		t.Fatalf("fallback logs = %q", logs)
	}
}

func TestUndeliveredAlerterFallsBackInsteadOfLeaking(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "terminal-args")
	alerter := writeScript(t, dir, "alerter", `
case "$1" in
  --version) echo 'alerter 26.5' ;;
  --list) exit 0 ;;
  *) trap 'exit 0' TERM; while :; do sleep 1; done ;;
esac
`)
	terminalNotifier := writeScript(t, dir, "terminal-notifier", `printf '%s\n' "$@" > "$TERMINAL_ARGS"`)
	t.Setenv("TERMINAL_ARGS", argsFile)

	n, err := importantNotification(kindApproval, "acme/api", 42, "https://github.com/acme/api/pull/42")
	if err != nil {
		t.Fatal(err)
	}
	if err := deliver(n, notifierTools{alerter: alerter, terminalNotifier: terminalNotifier}, nil, 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(argsFile); err != nil {
		t.Fatalf("temporary fallback was not sent: %v", err)
	}
}

func TestAlerterVersionRejectsTheLegacyCLI(t *testing.T) {
	dir := t.TempDir()
	legacy := writeScript(t, dir, "alerter", `echo 'alerter 1.0.1'`)
	if _, err := AlerterVersion(legacy); err == nil || !strings.Contains(err.Error(), "26.3 or newer") {
		t.Fatalf("AlerterVersion legacy error = %v", err)
	}
	oldBanner := writeScript(t, dir, "alerter-old-banner", `echo '26.2'`)
	if _, err := AlerterVersion(oldBanner); err == nil || !strings.Contains(err.Error(), "26.3 or newer") {
		t.Fatalf("AlerterVersion 26.2 error = %v", err)
	}
	minimum := writeScript(t, dir, "alerter-minimum", `echo '26.3'`)
	if got, err := AlerterVersion(minimum); err != nil || got != "26.3" {
		t.Fatalf("AlerterVersion minimum = %q, %v", got, err)
	}
	current := writeScript(t, dir, "alerter-current", `echo 'alerter 26.5'`)
	if got, err := AlerterVersion(current); err != nil || got != "alerter 26.5" {
		t.Fatalf("AlerterVersion current = %q, %v", got, err)
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

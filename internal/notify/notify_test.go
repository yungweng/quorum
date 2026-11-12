package notify

import (
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestActionNoticeIsClickableAndAudible(t *testing.T) {
	cmd := ReadyToMerge("acme/api", 42).command("/opt/homebrew/bin/terminal-notifier")
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

func TestApprovalAndReadyNoticesUseDifferentGroups(t *testing.T) {
	if ApprovalRequired("acme/api", 42).group == ReadyToMerge("acme/api", 42).group {
		t.Fatal("ready and approval notifications share a group, so one would replace the other")
	}
}

func TestRoutineNoticeIsSilentAndOpensOnlyAGivenURL(t *testing.T) {
	cmd := Routine("Reviewed api#42", "nothing found", "").command("terminal-notifier")
	want := []string{"terminal-notifier", "-title", "Reviewed api#42", "-message", "nothing found", "-group", "io.github.quorum"}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Fatalf("args = %q, want %q", cmd.Args, want)
	}
	withURL := Routine("Reviewed api#42", "nothing found", "https://example.invalid/42").command("terminal-notifier")
	if got := withURL.Args[len(withURL.Args)-2:]; !reflect.DeepEqual(got, []string{"-open", "https://example.invalid/42"}) {
		t.Fatalf("args = %q", withURL.Args)
	}
}

func TestSendReportsMissingNotifierInsteadOfFallingBack(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := Routine("Reviewed api#42", "nothing found", "").send()
	if runtime.GOOS != "darwin" {
		if err != nil {
			t.Fatalf("Send off macOS = %v, want nil", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), "terminal-notifier not found") {
		t.Fatalf("error = %v", err)
	}
}

func TestSendOrLogsAndFallsBackOnlyWhenSendFails(t *testing.T) {
	var logged, title, body string
	logf := func(format string, args ...any) { logged = fmt.Sprintf(format, args...) }
	fallback := func(t, b string) { title, body = t, b }

	ApprovalRequired("acme/api", 0).SendOr(logf, fallback)
	if runtime.GOOS != "darwin" {
		if logged != "" || title != "" {
			t.Fatalf("SendOr off macOS logged %q, fell back to %q", logged, title)
		}
		return
	}
	if !strings.HasPrefix(logged, "notification not sent: ") || title != "quorum: approval required" || body == "" {
		t.Fatalf("logged %q, fallback %q / %q", logged, title, body)
	}
	// Nil functions are allowed.
	ApprovalRequired("acme/api", 0).SendOr(nil, nil)
}

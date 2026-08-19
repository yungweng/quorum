package loop

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/envexec"
)

// The offline loop's whole point is that nothing reaches origin before the
// final push, so a session-side push would defeat it silently. The standing
// rules still carry the exact push command: the CI repair steps after the
// final push are the one place a step tells the session to push, and a bare
// `git push` fails on the detached checkout.
func TestStandingRulesOfflineDeferThePushToThePipeline(t *testing.T) {
	rules := standingRules("feature/crumb-tray", true, false, true, "")
	if !strings.Contains(rules, "Do not push. The pipeline pushes the branch itself") {
		t.Error("offline standing rules do not defer the push to the pipeline")
	}
	if !strings.Contains(rules, "git push origin HEAD:refs/heads/feature/crumb-tray") {
		t.Error("offline standing rules lost the exact push command CI repairs need")
	}
	if !strings.Contains(rules, "one push and one CI run at the very end") {
		t.Error("offline standing rules do not describe the offline pipeline")
	}

	online := standingRules("feature/crumb-tray", true, false, false, "")
	if !strings.Contains(online, "Only push when a step explicitly tells you to") {
		t.Error("online standing rules changed")
	}
}

func TestOfflineRoundPromptsForbidPushing(t *testing.T) {
	for name, prompt := range map[string]string{
		"fix round pr":      fixRoundPrompt(12, "", false, true, "## Summary\n\nfine"),
		"fix round branch":  fixRoundPrompt(0, "feature/crumb-tray", true, true, "## Summary\n\nfine"),
		"suggestion pr":     suggestionRoundPrompt(12, "", false, true, "## Summary\n\nfine"),
		"suggestion branch": suggestionRoundPrompt(0, "feature/crumb-tray", true, true, "## Summary\n\nfine"),
	} {
		if !strings.Contains(prompt, "Do not push") {
			t.Errorf("%s prompt does not forbid pushing", name)
		}
		if strings.Contains(prompt, ", and push") {
			t.Errorf("%s prompt still tells the session to push", name)
		}
		if strings.Contains(prompt, "Do not wait for CI") {
			t.Errorf("%s prompt mentions a CI wait no offline round has", name)
		}
	}
	conflict := conflictFixPrompt(12, "main", "feature/crumb-tray", false, true)
	if !strings.Contains(conflict, "Do not push") || strings.Contains(conflict, ", and push") {
		t.Errorf("offline conflict prompt still tells the session to push: %q", conflict)
	}
}

func TestTestFixPromptCarriesCommandOutputAndCommentContract(t *testing.T) {
	got := testFixPrompt("make check", "FAIL: TestCrumbTray")
	for _, want := range []string{
		"make check",
		"FAIL: TestCrumbTray",
		"Do not push",
		"a line that is exactly:\n" + MarkerComment,
		"never mention AI",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("test fix prompt is missing %q", want)
		}
	}
}

// The gate itself is deterministic: no configured command passes, a green
// command passes, and a red one with no repair budget left is ErrTestsRed
// rather than a silent push of failing code.
func TestEnsureTestsGreenGatesOnTheConfiguredCommand(t *testing.T) {
	dir := t.TempDir()
	r := &run{
		ctx:    context.Background(),
		rep:    NopReporter{},
		logDir: dir,
		env:    envexec.Env{Worktree: dir},
	}

	if err := r.ensureTestsGreen(); err != nil {
		t.Fatalf("no configured command must pass: %v", err)
	}

	r.o.TestCmd = "true"
	if err := r.ensureTestsGreen(); err != nil {
		t.Fatalf("a green command must pass: %v", err)
	}

	r.o.TestCmd = "false"
	r.o.MaxCIFixes = 0
	if err := r.ensureTestsGreen(); !errors.Is(err, ErrTestsRed) {
		t.Fatalf("err = %v, want ErrTestsRed", err)
	}
}

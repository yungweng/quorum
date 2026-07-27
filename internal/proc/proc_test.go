package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunReturnsTheCommandsExitStatus(t *testing.T) {
	err := Run(t.Context(), 0, Spec{Name: "sh", Args: []string{"-c", "exit 7"}})
	if got := ExitCode(err); got != 7 {
		t.Errorf("exit code = %d, want 7", got)
	}
}

func TestRunSucceeds(t *testing.T) {
	if err := Run(t.Context(), time.Minute, Spec{Name: "true"}); err != nil {
		t.Errorf("true failed: %v", err)
	}
}

// A timeout has to be distinguishable from an ordinary failure: the pipeline
// reports it differently, and 124 is what the shell tools produced.
func TestTimeoutIsReportedAsSuch(t *testing.T) {
	err := Run(t.Context(), 100*time.Millisecond, Spec{Name: "sleep", Args: []string{"30"}})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if got := ExitCode(err); got != TimeoutExitCode {
		t.Errorf("exit code = %d, want %d", got, TimeoutExitCode)
	}
}

// The reason process groups replaced the old pid-by-pid walk: a timeout has to
// reach the grandchildren too. Codex spawns MCP servers and toolchains below
// itself, and one survivor keeps doing whatever it was doing, unattended.
func TestTimeoutKillsGrandchildren(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "grandchild.pid")

	// A shell that backgrounds a long sleep, records its pid, and then blocks.
	// Killing only the shell would leave the sleep running.
	script := "sleep 300 & echo $! > " + pidFile + "; wait"
	err := Run(t.Context(), 300*time.Millisecond, Spec{Name: "sh", Args: []string{"-c", script}})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}

	data, readErr := os.ReadFile(pidFile)
	if readErr != nil {
		t.Skipf("the child never recorded its pid: %v", readErr)
	}
	pid, convErr := strconv.Atoi(strings.TrimSpace(string(data)))
	if convErr != nil {
		t.Skipf("unreadable pid file: %q", data)
	}

	// Signal 0 probes without sending anything. Give the group a moment to
	// finish dying before concluding it did not.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return // gone, which is the point
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Do not leave a stray sleep behind if the assertion fails.
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("grandchild %d survived the timeout", pid)
}

// Cancelling must tear the tree down the same way, because that is what a
// discarded review and a Ctrl-C both rely on.
func TestCancelKillsTheProcess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Run(ctx, 0, Spec{Name: "sleep", Args: []string{"30"}})
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

// A timeout of zero means no timeout, which is how `--fix-timeout 0` and
// `--review-timeout 0` disable it.
func TestZeroTimeoutDoesNotKill(t *testing.T) {
	start := time.Now()
	if err := Run(t.Context(), 0, Spec{Name: "sleep", Args: []string{"0.3"}}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) < 250*time.Millisecond {
		t.Error("the command was cut short although the timeout was disabled")
	}
}

func TestExitCodeOfAMissingBinary(t *testing.T) {
	err := Run(t.Context(), time.Minute, Spec{Name: "definitely-not-a-real-binary-xyz"})
	if err == nil {
		t.Fatal("a missing binary did not fail")
	}
	if ExitCode(err) == 0 {
		t.Error("a missing binary reported success")
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		t.Error("a missing binary was reported as an exit status")
	}
}

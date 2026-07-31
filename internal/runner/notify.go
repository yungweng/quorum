package runner

import (
	"fmt"
	"os/exec"
	"runtime"
)

// notify uses the terminal for foreground runs and terminal-notifier for the
// detached launchd agent. AppleScript is deliberately not a fallback: macOS
// attributes those notifications to Script Editor rather than to the terminal.
func (r *Runner) notify(title, body, url string) {
	if !r.Cfg.Notify {
		return
	}
	if r.TerminalNotify != nil {
		r.TerminalNotify(title, body)
		return
	}
	if runtime.GOOS != "darwin" {
		return
	}

	cmd, err := terminalNotifierCommand(title, body, url)
	if err != nil {
		r.logNotificationError(err)
		return
	}
	if err := cmd.Run(); err != nil {
		r.logNotificationError(err)
	}
}

func terminalNotifierCommand(title, body, url string) (*exec.Cmd, error) {
	bin, err := exec.LookPath("terminal-notifier")
	if err != nil {
		return nil, fmt.Errorf("terminal-notifier not found: %w", err)
	}
	args := []string{"-title", title, "-message", body, "-group", "io.github.quorum"}
	if url != "" {
		args = append(args, "-open", url)
	}
	return exec.Command(bin, args...), nil
}

func (r *Runner) logNotificationError(err error) {
	if r.Log != nil {
		r.Log.Printf("notification not sent: %v", err)
	}
}

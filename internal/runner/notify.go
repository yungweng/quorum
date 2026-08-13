package runner

import (
	"fmt"
	"os/exec"
	"runtime"

	macnotify "github.com/yungweng/quorum/internal/notify"
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

// notifyApprovalRequired always uses Notification Center, including for a
// foreground run. Its per-PR group is not replaced by routine completion
// notifications, so the action stays visible until the user dismisses it.
func (r *Runner) notifyApprovalRequired(repo string, number int) {
	if !r.Cfg.Notify {
		return
	}
	url := fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
	if err := macnotify.ApprovalRequired(repo, number, url); err != nil {
		r.logNotificationError(err)
		if r.TerminalNotify != nil {
			r.TerminalNotify("quorum: approval required",
				fmt.Sprintf("%s#%d is clean. Ask another reviewer to approve it.", repo, number))
		}
	}
}

// notifyReadyToMerge mirrors notifyApprovalRequired: a persistent, clickable
// Notification Center item per pull request whose clean review was not merged.
func (r *Runner) notifyReadyToMerge(repo string, number int) {
	if !r.Cfg.Notify {
		return
	}
	url := fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
	if err := macnotify.ReadyToMerge(repo, number, url); err != nil {
		r.logNotificationError(err)
		if r.TerminalNotify != nil {
			r.TerminalNotify("quorum: ready to merge",
				fmt.Sprintf("%s#%d is clean and ready to merge.", repo, number))
		}
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

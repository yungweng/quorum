// Package notify sends macOS notifications through osascript. Notification
// Center attributes them to Script Editor; that is the price of the one
// mechanism macOS 26 still displays. terminal-notifier's deprecated
// NSUserNotification path is accepted by the system but never shown.
package notify

import (
	"fmt"
	"os/exec"
	"runtime"
)

// ApprovalRequired alerts, with sound, that a pull request needs another
// reviewer's approval.
func ApprovalRequired(repo string, number int) error {
	return send("quorum: approval required",
		fmt.Sprintf("%s#%d is clean. Ask another reviewer to approve it.", repo, number), true)
}

// ReadyToMerge alerts, with sound, that a pull request's clean review was not
// auto-merged.
func ReadyToMerge(repo string, number int) error {
	return send("quorum: ready to merge",
		fmt.Sprintf("%s#%d is clean and ready to merge.", repo, number), true)
}

// Send posts a routine notification without sound.
func Send(title, body string) error {
	return send(title, body, false)
}

func send(title, body string, sound bool) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return command(title, body, sound).Run()
}

// command passes title and body as arguments rather than splicing them into
// the script, so they need no AppleScript quoting.
func command(title, body string, sound bool) *exec.Cmd {
	script := "display notification (item 2 of argv) with title (item 1 of argv)"
	if sound {
		script += ` sound name "default"`
	}
	return exec.Command("/usr/bin/osascript",
		"-e", "on run argv", "-e", script, "-e", "end run", title, body)
}

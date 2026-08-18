package runner

import (
	"fmt"
	"runtime"

	macnotify "github.com/yungweng/quorum/internal/notify"
)

// notify uses the terminal for foreground runs and Notification Center for
// the detached launchd agent.
func (r *Runner) notify(title, body string) {
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
	if err := macnotify.Send(title, body); err != nil {
		r.logNotificationError(err)
	}
}

// notifyApprovalRequired always uses Notification Center, including for a
// foreground run, so the alert outlives the terminal session.
func (r *Runner) notifyApprovalRequired(repo string, number int) {
	if !r.Cfg.Notify {
		return
	}
	if err := macnotify.ApprovalRequired(repo, number); err != nil {
		r.logNotificationError(err)
		if r.TerminalNotify != nil {
			r.TerminalNotify("quorum: approval required",
				fmt.Sprintf("%s#%d is clean. Ask another reviewer to approve it.", repo, number))
		}
	}
}

// notifyReadyToMerge mirrors notifyApprovalRequired for a pull request whose
// clean review was not merged.
func (r *Runner) notifyReadyToMerge(repo string, number int) {
	if !r.Cfg.Notify {
		return
	}
	if err := macnotify.ReadyToMerge(repo, number); err != nil {
		r.logNotificationError(err)
		if r.TerminalNotify != nil {
			r.TerminalNotify("quorum: ready to merge",
				fmt.Sprintf("%s#%d is clean and ready to merge.", repo, number))
		}
	}
}

func (r *Runner) logNotificationError(err error) {
	if r.Log != nil {
		r.Log.Printf("notification not sent: %v", err)
	}
}

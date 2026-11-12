package runner

import (
	macnotify "github.com/yungweng/quorum/internal/notify"
)

// notify sends a routine completion notice: to the terminal for foreground
// runs, to Notification Center for the detached launchd agent.
func (r *Runner) notify(title, body, url string) {
	if !r.Cfg.Notify {
		return
	}
	if r.TerminalNotify != nil {
		r.TerminalNotify(title, body)
		return
	}
	macnotify.Routine(title, body, url).SendOr(r.logf(), nil)
}

// notifyAction always uses Notification Center, including for a foreground
// run, and only falls back to the terminal when that fails.
func (r *Runner) notifyAction(n macnotify.Notice) {
	if !r.Cfg.Notify {
		return
	}
	n.SendOr(r.logf(), r.TerminalNotify)
}

func (r *Runner) logf() func(string, ...any) {
	if r.Log == nil {
		return nil
	}
	return r.Log.Printf
}

package cli

import (
	macnotify "github.com/yungweng/quorum/internal/notify"
)

// notifyAction sends a notice that needs the user to act, falling back to the
// terminal when Notification Center cannot deliver it.
func (a *app) notifyAction(enabled bool, n macnotify.Notice) {
	if !enabled {
		return
	}
	var logf func(string, ...any)
	if a.log != nil {
		logf = a.log.Printf
	}
	var fallback func(string, string)
	if a.out != nil {
		fallback = a.out.Notify
	}
	n.SendOr(logf, fallback)
}

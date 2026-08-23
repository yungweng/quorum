package cli

import (
	"fmt"
	"strconv"

	macnotify "github.com/yungweng/quorum/internal/notify"
)

// cmdNotify is the detached child that owns one persistent alert until the
// user opens its pull request or dismisses it.
func (a *app) cmdNotify(args []string) int {
	if len(args) != 4 {
		return a.die("_notify is internal and takes kind repo number url")
	}
	number, err := strconv.Atoi(args[2])
	if err != nil {
		return a.die("_notify PR number: %v", err)
	}
	var logf func(string, ...any)
	if a.log != nil {
		logf = a.log.Printf
	}
	if err := macnotify.DeliverImportant(args[0], args[1], number, args[3], logf); err != nil {
		if logf != nil {
			logf("notification not sent: %v", err)
		}
		return exitError
	}
	return exitOK
}

func (a *app) notifyReadyToMerge(enabled bool, repo string, number int, url string) {
	if !enabled {
		return
	}
	if url == "" {
		url = fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
	}
	if err := macnotify.ReadyToMerge(repo, number, url); err != nil {
		if a.log != nil {
			a.log.Printf("notification not sent: %v", err)
		}
		if a.out != nil {
			a.out.Notify("quorum: ready to merge",
				fmt.Sprintf("%s#%d is clean and ready to merge.", repo, number))
		}
	}
}

func (a *app) notifyApprovalRequired(enabled bool, repo string, number int, url string) {
	if !enabled {
		return
	}
	if url == "" {
		url = fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
	}
	if err := macnotify.ApprovalRequired(repo, number, url); err != nil {
		if a.log != nil {
			a.log.Printf("notification not sent: %v", err)
		}
		if a.out != nil {
			a.out.Notify("quorum: approval required",
				fmt.Sprintf("%s#%d is clean. Ask another reviewer to approve it.", repo, number))
		}
	}
}

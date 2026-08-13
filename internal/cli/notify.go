package cli

import (
	"fmt"

	macnotify "github.com/yungweng/quorum/internal/notify"
)

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

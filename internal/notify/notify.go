// Package notify sends macOS notifications that need an explicit user action.
// Routine completion notifications remain with their callers.
package notify

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
)

const (
	kindApproval = "approval"
	kindReady    = "ready"
)

type important struct {
	title string
	body  string
	group string
	url   string
}

func ApprovalRequired(repo string, number int, url string) error {
	return send(kindApproval, repo, number, url)
}

func ReadyToMerge(repo string, number int, url string) error {
	return send(kindReady, repo, number, url)
}

func send(kind, repo string, number int, url string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	n, err := importantNotification(kind, repo, number, url)
	if err != nil {
		return err
	}
	bin, err := exec.LookPath("terminal-notifier")
	if err != nil {
		return fmt.Errorf("terminal-notifier not found: %w", err)
	}
	return deliver(n, bin)
}

func importantNotification(kind, repo string, number int, url string) (important, error) {
	if repo == "" || number <= 0 || url == "" {
		return important{}, errors.New("important notification needs repo, positive PR number and URL")
	}
	n := important{url: url}
	switch kind {
	case kindApproval:
		n.title = "quorum: approval required"
		n.body = fmt.Sprintf("%s#%d is clean. Ask another reviewer to approve it.", repo, number)
		n.group = fmt.Sprintf("io.github.quorum.approval.%s#%d", repo, number)
	case kindReady:
		n.title = "quorum: ready to merge"
		n.body = fmt.Sprintf("%s#%d is clean and ready to merge.", repo, number)
		n.group = fmt.Sprintf("io.github.quorum.ready.%s#%d", repo, number)
	default:
		return important{}, fmt.Errorf("unknown important notification kind %q", kind)
	}
	return n, nil
}

func deliver(n important, bin string) error {
	return terminalNotifierCommand(bin, n).Run()
}

func terminalNotifierCommand(bin string, n important) *exec.Cmd {
	return exec.Command(bin,
		"-title", n.title,
		"-message", n.body,
		"-group", n.group,
		"-sound", "default",
		"-open", n.url,
	)
}

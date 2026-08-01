// Package notify sends macOS notifications that must survive routine quorum
// completion notifications.
package notify

import (
	"fmt"
	"os/exec"
	"runtime"
)

// ApprovalRequired leaves one Notification Center item per pull request.
// Routine notifications use a different group, so they cannot replace it.
func ApprovalRequired(repo string, number int, url string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	title := "quorum: approval required"
	body := fmt.Sprintf("%s#%d is clean. Ask another reviewer to approve it.", repo, number)
	cmd, err := approvalRequiredCommand(title, body, url, approvalGroup(repo, number))
	if err != nil {
		return err
	}
	return cmd.Run()
}

func approvalGroup(repo string, number int) string {
	return fmt.Sprintf("io.github.quorum.approval.%s#%d", repo, number)
}

func approvalRequiredCommand(title, body, url, group string) (*exec.Cmd, error) {
	bin, err := exec.LookPath("terminal-notifier")
	if err != nil {
		return nil, fmt.Errorf("terminal-notifier not found: %w", err)
	}
	args := []string{
		"-title", title,
		"-message", body,
		"-group", group,
		"-sound", "default",
	}
	if url != "" {
		args = append(args, "-open", url)
	}
	return exec.Command(bin, args...), nil
}

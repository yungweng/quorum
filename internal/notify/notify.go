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
	return persistent(
		"quorum: approval required",
		fmt.Sprintf("%s#%d is clean. Ask another reviewer to approve it.", repo, number),
		url, approvalGroup(repo, number))
}

// ReadyToMerge leaves one Notification Center item per pull request whose
// clean review was not auto-merged. Clicking it opens the pull request.
func ReadyToMerge(repo string, number int, url string) error {
	return persistent(
		"quorum: ready to merge",
		fmt.Sprintf("%s#%d is clean and ready to merge.", repo, number),
		url, readyGroup(repo, number))
}

func persistent(title, body, url, group string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	cmd, err := persistentCommand(title, body, url, group)
	if err != nil {
		return err
	}
	return cmd.Run()
}

func approvalGroup(repo string, number int) string {
	return fmt.Sprintf("io.github.quorum.approval.%s#%d", repo, number)
}

func readyGroup(repo string, number int) string {
	return fmt.Sprintf("io.github.quorum.ready.%s#%d", repo, number)
}

func persistentCommand(title, body, url, group string) (*exec.Cmd, error) {
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

// Package notify sends macOS notifications through terminal-notifier. That
// tool is the only sender: AppleScript is deliberately not a fallback, because
// macOS attributes those notifications to Script Editor rather than to the
// terminal.
package notify

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"

	"github.com/yungweng/quorum/internal/prkey"
)

// Notice is one notification. Title and Body are exported so a caller whose
// notification could not be delivered can show the same text another way.
type Notice struct {
	Title string
	Body  string

	group string
	url   string
	// sound marks a notice that needs an explicit user action.
	sound bool
	err   error
}

// routineGroup is shared by all routine notices, so each replaces the last.
const routineGroup = "io.github.quorum"

// Routine is a completion notice. A click opens url when there is one.
func Routine(title, body, url string) Notice {
	return Notice{Title: title, Body: body, group: routineGroup, url: url}
}

// ApprovalRequired tells the user that a clean pull request waits for another
// reviewer. Its per-PR group is not replaced by routine completion notices.
func ApprovalRequired(repo string, number int) Notice {
	return actionNotice(repo, number, "approval", "quorum: approval required",
		"%s#%d is clean. Ask another reviewer to approve it.")
}

// ReadyToMerge tells the user that a clean pull request was not merged.
func ReadyToMerge(repo string, number int) Notice {
	return actionNotice(repo, number, "ready", "quorum: ready to merge",
		"%s#%d is clean and ready to merge.")
}

func actionNotice(repo string, number int, kind, title, body string) Notice {
	n := Notice{
		Title: title,
		Body:  fmt.Sprintf(body, repo, number),
		group: fmt.Sprintf("io.github.quorum.%s.%s#%d", kind, repo, number),
		url:   prkey.URL(repo, number),
		sound: true,
	}
	if repo == "" || number <= 0 {
		n.err = errors.New("important notification needs repo and positive PR number")
	}
	return n
}

// Anywhere but macOS, send does nothing.
func (n Notice) send() error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	if n.err != nil {
		return n.err
	}
	bin, err := exec.LookPath("terminal-notifier")
	if err != nil {
		return fmt.Errorf("terminal-notifier not found: %w", err)
	}
	return n.command(bin).Run()
}

// SendOr sends n. When that fails it logs why through logf and hands the
// notice to fallback, typically a terminal notification. Either may be nil.
func (n Notice) SendOr(logf func(format string, args ...any), fallback func(title, body string)) {
	err := n.send()
	if err == nil {
		return
	}
	if logf != nil {
		logf("notification not sent: %v", err)
	}
	if fallback != nil {
		fallback(n.Title, n.Body)
	}
}

func (n Notice) command(bin string) *exec.Cmd {
	args := []string{"-title", n.Title, "-message", n.Body, "-group", n.group}
	if n.sound {
		args = append(args, "-sound", "default")
	}
	if n.url != "" {
		args = append(args, "-open", n.url)
	}
	return exec.Command(bin, args...)
}

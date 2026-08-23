// Package notify sends the macOS notifications that need an explicit user
// action. Routine completion notifications remain with their callers.
package notify

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	kindApproval = "approval"
	kindReady    = "ready"

	alerterBundleID = "fr.vjeantet.alerter"
	openAction      = "Open PR"
	dismissAction   = "Dismiss"

	deliveryProbeInterval = 100 * time.Millisecond
	deliveryProbeTimeout  = 5 * time.Second
	probeCommandTimeout   = 500 * time.Millisecond
	stopGrace             = time.Second
)

type important struct {
	kind  string
	title string
	body  string
	group string
	url   string
	repo  string
	num   int
}

type notifierTools struct {
	alerter          string
	terminalNotifier string
	opener           string
}

// ApprovalRequired leaves an alert that stays until the user opens the pull
// request or dismisses it.
func ApprovalRequired(repo string, number int, url string) error {
	return startImportant(kindApproval, repo, number, url)
}

// ReadyToMerge leaves an alert that stays until the user opens the pull
// request or dismisses it.
func ReadyToMerge(repo string, number int, url string) error {
	return startImportant(kindReady, repo, number, url)
}

// DeliverImportant is the blocking half of an important notification. The
// CLI runs it in a detached child so a persistent alert never holds a review
// open.
func DeliverImportant(kind, repo string, number int, url string, logf func(string, ...any)) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	n, err := importantNotification(kind, repo, number, url)
	if err != nil {
		return err
	}
	return deliver(n, resolveTools(), logf, deliveryProbeTimeout)
}

func startImportant(kind, repo string, number int, url string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	n, err := importantNotification(kind, repo, number, url)
	if err != nil {
		return err
	}
	t := resolveTools()
	if err := canStart(t); err != nil {
		return err
	}

	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find quorum executable: %w", err)
	}
	return startChild(self, n)
}

func canStart(t notifierTools) error {
	if t.alerter != "" {
		if _, err := AlerterVersion(t.alerter); err == nil {
			return nil
		} else if t.terminalNotifier == "" {
			return fmt.Errorf("persistent alerts unavailable: %w; terminal-notifier fallback not found", err)
		}
	}
	if t.terminalNotifier != "" {
		return nil
	}
	return errors.New("persistent alerts unavailable: alerter and terminal-notifier not found")
}

func childCommand(self string, n important) *exec.Cmd {
	cmd := exec.Command(self, "_notify", n.kind, n.repo, strconv.Itoa(n.num), n.url)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return cmd
}

func startChild(self string, n important) error {
	cmd := childCommand(self, n)
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", os.DevNull, err)
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	if err := cmd.Start(); err != nil {
		devnull.Close()
		return fmt.Errorf("start persistent alert: %w", err)
	}
	devnull.Close()
	_ = cmd.Process.Release()
	return nil
}

func importantNotification(kind, repo string, number int, url string) (important, error) {
	if repo == "" || number <= 0 || url == "" {
		return important{}, errors.New("important notification needs repo, positive PR number and URL")
	}
	n := important{kind: kind, repo: repo, num: number, url: url}
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

func resolveTools() notifierTools {
	t := notifierTools{opener: "/usr/bin/open"}
	t.alerter, _ = exec.LookPath("alerter")
	t.terminalNotifier, _ = exec.LookPath("terminal-notifier")
	return t
}

func deliver(n important, t notifierTools, logf func(string, ...any), probeTimeout time.Duration) error {
	primaryErr := errors.New("alerter not found")
	if t.alerter != "" {
		if _, err := AlerterVersion(t.alerter); err != nil {
			primaryErr = err
		} else {
			result, err := runAlerter(t.alerter, n, probeTimeout)
			if err == nil {
				if !requestsOpen(result) {
					return nil
				}
				if t.opener == "" {
					return errors.New("open pull request: opener not found")
				}
				if err := exec.Command(t.opener, n.url).Run(); err != nil {
					return fmt.Errorf("open pull request: %w", err)
				}
				return nil
			}
			primaryErr = err
		}
	}

	if t.terminalNotifier == "" {
		return fmt.Errorf("persistent alert unavailable: %v; temporary fallback unavailable: terminal-notifier not found", primaryErr)
	}
	if err := terminalNotifierCommand(t.terminalNotifier, n).Run(); err != nil {
		return fmt.Errorf("persistent alert unavailable: %v; temporary fallback failed: %w", primaryErr, err)
	}
	if logf != nil {
		logf("persistent alert unavailable: %v; sent temporary fallback", primaryErr)
	}
	return nil
}

func alerterCommand(bin string, n important) *exec.Cmd {
	return exec.Command(bin,
		"--title", n.title,
		"--message", n.body,
		"--group", n.group,
		"--sender", alerterBundleID,
		"--sound", "default",
		"--actions", openAction,
		"--close-label", dismissAction,
	)
}

func runAlerter(bin string, n important, probeTimeout time.Duration) (string, error) {
	cmd := alerterCommand(bin, n)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start alerter: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	ticker := time.NewTicker(deliveryProbeInterval)
	defer ticker.Stop()
	timer := time.NewTimer(probeTimeout)
	defer timer.Stop()
	for {
		select {
		case err := <-done:
			return alerterResult(stdout.String(), stderr.String(), err)
		case <-ticker.C:
			if notificationListed(bin, n.group) {
				ticker.Stop()
				timer.Stop()
				err := <-done
				return alerterResult(stdout.String(), stderr.String(), err)
			}
		case <-timer.C:
			stop(cmd, done)
			return "", errors.New("alerter did not deliver the notification")
		}
	}
}

func notificationListed(bin, group string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeCommandTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "--list", group).Output()
	return err == nil && strings.TrimSpace(string(out)) != ""
}

func stop(cmd *exec.Cmd, done <-chan error) {
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-done:
		return
	case <-time.After(stopGrace):
		_ = cmd.Process.Kill()
		<-done
	}
}

func alerterResult(stdout, stderr string, err error) (string, error) {
	if err == nil {
		return strings.TrimSpace(stdout), nil
	}
	detail := strings.TrimSpace(stderr)
	if detail == "" {
		return "", fmt.Errorf("alerter failed: %w", err)
	}
	return "", fmt.Errorf("alerter failed: %s: %w", detail, err)
}

func requestsOpen(result string) bool {
	switch strings.TrimSpace(result) {
	case openAction, "@CONTENTCLICKED", "@ACTIONCLICKED":
		return true
	default:
		return false
	}
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

// AlerterVersion returns the installed version line and rejects versions from
// before 26.3, which introduced persistent alert-style notifications.
func AlerterVersion(path string) (string, error) {
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("alerter --version: %w", err)
	}
	line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return "", errors.New("alerter returned no version")
	}
	version := strings.TrimPrefix(fields[len(fields)-1], "v")
	parts := strings.Split(version, ".")
	major, err := strconv.Atoi(parts[0])
	if err != nil || major < 26 {
		return "", fmt.Errorf("alerter 26.3 or newer required, got %q", line)
	}
	if major == 26 {
		if len(parts) < 2 {
			return "", fmt.Errorf("alerter 26.3 or newer required, got %q", line)
		}
		minor, err := strconv.Atoi(parts[1])
		if err != nil || minor < 3 {
			return "", fmt.Errorf("alerter 26.3 or newer required, got %q", line)
		}
	}
	return line, nil
}

package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yungweng/quorum/internal/config"
)

func TestNotificationChecksUseTerminalNotifierForEveryNotification(t *testing.T) {
	dir := t.TempDir()
	writeDoctorTool(t, dir, "terminal-notifier", "terminal-notifier 2.0.0")
	t.Setenv("PATH", dir)

	checks := notificationChecks(config.Config{Notify: true, NotifyReadyToMerge: true, AutoMerge: true})
	c, ok := findCheck(checks, "notifications")
	if len(checks) != 1 || !ok || c.level != 0 || c.detail != "terminal-notifier 2.0.0" {
		t.Fatalf("notification checks = %+v", checks)
	}
}

func writeDoctorTool(t *testing.T, dir, name, version string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+version+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func findCheck(checks []check, name string) (check, bool) {
	for _, c := range checks {
		if c.name == name {
			return c, true
		}
	}
	return check{}, false
}

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/config"
)

func TestNotificationChecksRequireAlerterOnlyForImportantAlerts(t *testing.T) {
	dir := t.TempDir()
	writeDoctorTool(t, dir, "terminal-notifier", "terminal-notifier 2.0.0")
	t.Setenv("PATH", dir)

	checks := notificationChecks(config.Config{Notify: true})
	if hasCheck(checks, "persistent alerts") {
		t.Fatalf("persistent alert check shown while no important alert is enabled: %+v", checks)
	}

	checks = notificationChecks(config.Config{Notify: true, NotifyReadyToMerge: true})
	c, ok := findCheck(checks, "persistent alerts")
	if !ok || c.level != 1 || !strings.Contains(c.fix, "vjeantet/tap/alerter") {
		t.Fatalf("missing alerter check = %+v, found = %v", c, ok)
	}

	checks = notificationChecks(config.Config{Notify: true, AutoMerge: true})
	if c, ok := findCheck(checks, "persistent alerts"); !ok || c.level != 1 {
		t.Fatalf("auto-merge did not require persistent alerts: %+v", checks)
	}
}

func TestNotificationChecksRejectLegacyAlerter(t *testing.T) {
	dir := t.TempDir()
	writeDoctorTool(t, dir, "terminal-notifier", "terminal-notifier 2.0.0")
	writeDoctorTool(t, dir, "alerter", "alerter 1.0.1")
	t.Setenv("PATH", dir)

	checks := notificationChecks(config.Config{Notify: true, NotifyReadyToMerge: true})
	c, ok := findCheck(checks, "persistent alerts")
	if !ok || c.level != 1 || !strings.Contains(c.detail, "26.3 or newer") {
		t.Fatalf("legacy alerter check = %+v, found = %v", c, ok)
	}
}

func TestNotificationChecksAcceptCurrentAlerter(t *testing.T) {
	dir := t.TempDir()
	writeDoctorTool(t, dir, "terminal-notifier", "terminal-notifier 2.0.0")
	writeDoctorTool(t, dir, "alerter", "alerter 26.5")
	t.Setenv("PATH", dir)

	checks := notificationChecks(config.Config{Notify: true, NotifyReadyToMerge: true})
	c, ok := findCheck(checks, "persistent alerts")
	if !ok || c.level != 0 || c.detail != "alerter 26.5" {
		t.Fatalf("current alerter check = %+v, found = %v", c, ok)
	}
}

func writeDoctorTool(t *testing.T, dir, name, version string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+version+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func hasCheck(checks []check, name string) bool {
	_, ok := findCheck(checks, name)
	return ok
}

func findCheck(checks []check, name string) (check, bool) {
	for _, c := range checks {
		if c.name == name {
			return c, true
		}
	}
	return check{}, false
}

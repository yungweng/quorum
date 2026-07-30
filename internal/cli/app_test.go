package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/paths"
	"github.com/yungweng/quorum/internal/ui"
)

func TestNoCommandShowsPrimaryCommandHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := &app{
		version: "1.2.3",
		cfg:     config.Config{PollInterval: 300},
		p:       paths.P{Config: "/tmp/quorum-config"},
		out:     ui.New(os.Stdout).To(&stdout),
		err:     ui.New(os.Stderr).To(&stderr),
	}

	if code := a.run(nil); code != exitOK {
		t.Fatalf("run(nil) exit code = %d, want %d", code, exitOK)
	}
	got := stdout.String()
	for _, want := range []string{
		"Usage:\n  quorum <command> [options]",
		"Main commands:",
		"quorum watch",
		"quorum review [pr]",
		"quorum babysit [pr]",
		"[pr] defaults to the pull request for the current branch.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("help is missing %q:\n%s", want, got)
		}
	}
	if stderr.Len() != 0 {
		t.Errorf("run(nil) wrote to stderr: %s", stderr.String())
	}
}

func TestWidenPathDoesNotRunNPMWhenSupportedToolsArePresent(t *testing.T) {
	dir := t.TempDir()
	called := filepath.Join(dir, "npm-called")
	for name, script := range map[string]string{
		"codex":  "#!/bin/sh\nexit 0\n",
		"direnv": "#!/bin/sh\nexit 0\n",
		"gh":     "#!/bin/sh\nexit 0\n",
		"git":    "#!/bin/sh\nexit 0\n",
		"npm":    "#!/bin/sh\n: > \"$WIDEN_PATH_NPM_CALLED\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("WIDEN_PATH_NPM_CALLED", called)

	widenPath()

	if _, err := os.Stat(called); !os.IsNotExist(err) {
		t.Fatalf("npm was run even though every supported tool was already on PATH: %v", err)
	}
}

func TestFindToolsUsesNPMPrefixForMissingRequiredTool(t *testing.T) {
	pathDir := t.TempDir()
	prefix := t.TempDir()
	prefixBin := filepath.Join(prefix, "bin")
	if err := os.Mkdir(prefixBin, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, script := range map[string]string{
		filepath.Join(pathDir, "codex"): "#!/bin/sh\nexit 0\n",
		filepath.Join(pathDir, "git"):   "#!/bin/sh\nexit 0\n",
		filepath.Join(pathDir, "npm"):   "#!/bin/sh\nprintf '%s\\n' \"$WIDEN_PATH_NPM_PREFIX\"\n",
		filepath.Join(prefixBin, "gh"):  "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", pathDir)
	t.Setenv("WIDEN_PATH_NPM_PREFIX", prefix)

	resolved, err := (&app{}).findTools()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.GH != filepath.Join(prefixBin, "gh") {
		t.Fatalf("gh path = %q, want npm-prefix path %q", resolved.GH, filepath.Join(prefixBin, "gh"))
	}
}

func TestFindToolsUsesNPMPrefixForDirenv(t *testing.T) {
	pathDir := t.TempDir()
	prefix := t.TempDir()
	prefixBin := filepath.Join(prefix, "bin")
	if err := os.Mkdir(prefixBin, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, script := range map[string]string{
		filepath.Join(pathDir, "codex"):    "#!/bin/sh\nexit 0\n",
		filepath.Join(pathDir, "gh"):       "#!/bin/sh\nexit 0\n",
		filepath.Join(pathDir, "git"):      "#!/bin/sh\nexit 0\n",
		filepath.Join(pathDir, "npm"):      "#!/bin/sh\nprintf '%s\\n' \"$WIDEN_PATH_NPM_PREFIX\"\n",
		filepath.Join(prefixBin, "direnv"): "#!/bin/sh\nexit 0\n",
	} {
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", pathDir)
	t.Setenv("WIDEN_PATH_NPM_PREFIX", prefix)

	resolved, err := (&app{}).findTools()
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Direnv != filepath.Join(prefixBin, "direnv") {
		t.Fatalf("direnv path = %q, want npm-prefix path %q", resolved.Direnv, filepath.Join(prefixBin, "direnv"))
	}
}

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/config"
	"github.com/yungweng/quorum/internal/engine/spec"
	"github.com/yungweng/quorum/internal/paths"
)

func TestNoCommandShowsPrimaryCommandHelp(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := &app{
		version: "1.2.3",
		cfg:     config.Config{PollInterval: 300},
		p:       paths.P{Config: "/tmp/quorum-config"},
		out:     plainWriter(&stdout),
		err:     plainWriter(&stderr),
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
		"Without [pr], an open PR for the current branch is used when one exists;",
		"otherwise quorum works on the pushed branch without PR comments or PR CI.",
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
	// Every engine spec knows about, so an engine added later counts as present.
	scripts := map[string]string{
		"direnv": "#!/bin/sh\nexit 0\n",
		"gh":     "#!/bin/sh\nexit 0\n",
		"git":    "#!/bin/sh\nexit 0\n",
		"npm":    "#!/bin/sh\n: > \"$WIDEN_PATH_NPM_CALLED\"\n",
	}
	for _, e := range spec.All {
		scripts[e.Bin] = "#!/bin/sh\nexit 0\n"
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	isolateHome(t)
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
	isolateHome(t)
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
	isolateHome(t)
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

// isolateHome points HOME at an empty directory. widenPath appends the agent
// directories under HOME, so the developer's own ~/.local/bin and ~/bin would
// otherwise take part in tool resolution.
func isolateHome(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
}

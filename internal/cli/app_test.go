package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWidenPathDoesNotRunNPMWhenRequiredToolsArePresent(t *testing.T) {
	dir := t.TempDir()
	called := filepath.Join(dir, "npm-called")
	for name, script := range map[string]string{
		"codex": "#!/bin/sh\nexit 0\n",
		"gh":    "#!/bin/sh\nexit 0\n",
		"git":   "#!/bin/sh\nexit 0\n",
		"npm":   "#!/bin/sh\n: > \"$WIDEN_PATH_NPM_CALLED\"\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("WIDEN_PATH_NPM_CALLED", called)

	widenPath()

	if _, err := os.Stat(called); !os.IsNotExist(err) {
		t.Fatalf("npm was run even though every required tool was already on PATH: %v", err)
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

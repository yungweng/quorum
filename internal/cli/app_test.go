package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWidenPathDoesNotRunNPMWhenCodexIsAlreadyPresent(t *testing.T) {
	dir := t.TempDir()
	codex := filepath.Join(dir, "codex")
	npm := filepath.Join(dir, "npm")
	called := filepath.Join(dir, "npm-called")
	for path, script := range map[string]string{
		codex: "#!/bin/sh\nexit 0\n",
		npm:   "#!/bin/sh\n: > \"$WIDEN_PATH_NPM_CALLED\"\n",
	} {
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir)
	t.Setenv("WIDEN_PATH_NPM_CALLED", called)

	widenPath()

	if _, err := os.Stat(called); !os.IsNotExist(err) {
		t.Fatalf("npm was run even though Codex was already on PATH: %v", err)
	}
}

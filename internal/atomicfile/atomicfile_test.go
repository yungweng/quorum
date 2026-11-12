package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteJSONReplacesTheFileAndLeavesNoTemporary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteJSON(path, map[string]int{"schema": 1}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "{\n  \"schema\": 1\n}\n"; string(got) != want {
		t.Errorf("file = %q, want %q", got, want)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want only the file", len(entries))
	}
}

func TestWriteKeepsTheBytesAndTheMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	data := []byte("{\"a\":1}\n{\"b\":2}\n")
	if err := Write(path, data); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Errorf("file = %q, want %q", got, data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&^0o022 != 0o644&^0o022 {
		t.Errorf("mode = %v, want 0644 less the umask", mode)
	}
}

func TestWriteFailsWithoutTheDirectory(t *testing.T) {
	if err := Write(filepath.Join(t.TempDir(), "missing", "file"), []byte("x")); err == nil {
		t.Fatal("Write into a missing directory succeeded")
	}
}

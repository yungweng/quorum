// Package atomicfile replaces small files in one rename, so a concurrent
// reader sees either the previous file or the new one, never a partial write.
package atomicfile

import (
	"encoding/json"
	"fmt"
	"os"
)

// WriteJSON writes value as indented JSON with a trailing newline to path,
// the way Write does.
func WriteJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return Write(path, append(b, '\n'))
}

// Write replaces path with data, mode 0644. The content goes to a
// process-specific temporary file beside path first, so two processes
// replacing the same file never write through one temporary. The directory
// must exist.
func Write(path string, data []byte) error {
	tmp := fmt.Sprintf("%s.tmp.%d", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

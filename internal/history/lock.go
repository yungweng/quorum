package history

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// lock serialises writers. Several runs can finish at the same moment, and an
// append is a read, a trim and a rewrite of the whole file, so two of them
// interleaving would drop entries.
//
// Readers take no lock. They can only ever see a complete file, because a
// write lands through a rename.
func lock(path string) (func(), error) {
	lockPath := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, errors.New("could not lock " + lockPath + ": " + err.Error())
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

package rundir

import (
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBranchRunDirsDistinguishFlattenedNames(t *testing.T) {
	runs := t.TempDir()
	withSlash := New(runs, "acme/api", 0, "foo/bar")
	withDash := New(runs, "acme/api", 0, "foo-bar")
	if withSlash == withDash {
		t.Fatalf("colliding branch run directories: %q", withSlash)
	}
}

// A directory component may not exceed 255 bytes, so a long branch name has to
// be cut, and never in the middle of a UTF-8 rune.
func TestBranchRunDirBoundsTheReadablePrefix(t *testing.T) {
	runs := t.TempDir()
	short := filepath.Base(New(runs, "acme/api", 0, "x"))

	long := filepath.Base(New(runs, "acme/api", 0, strings.Repeat("crumb/", 100)))
	if len(long) > len(short)-1+maxReadableBytes {
		t.Fatalf("branch run directory is %d bytes: %q", len(long), long)
	}

	unicode := filepath.Base(New(runs, "acme/api", 0, strings.Repeat("ä", 100)))
	if !utf8.ValidString(unicode) {
		t.Fatalf("branch run directory cuts a UTF-8 rune: %q", unicode)
	}
}

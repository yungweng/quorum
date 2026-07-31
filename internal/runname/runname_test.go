package runname

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestBranchPartDistinguishesFlattenedNames(t *testing.T) {
	withSlash := BranchPart("foo/bar")
	withDash := BranchPart("foo-bar")
	if withSlash == withDash {
		t.Fatalf("colliding branch run names: %q", withSlash)
	}
}

func TestBranchPartBoundsTheReadablePrefix(t *testing.T) {
	part := BranchPart(strings.Repeat("crumb/", 100))
	if len(part) > maxReadableBytes+1+32 {
		t.Fatalf("branch run part is %d bytes: %q", len(part), part)
	}

	unicodePart := BranchPart(strings.Repeat("ä", 100))
	if !utf8.ValidString(unicodePart) {
		t.Fatalf("branch run part cuts a UTF-8 rune: %q", unicodePart)
	}
}

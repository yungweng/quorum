package runname

import "testing"

func TestBranchPartDistinguishesFlattenedNames(t *testing.T) {
	withSlash := BranchPart("foo/bar")
	withDash := BranchPart("foo-bar")
	if withSlash == withDash {
		t.Fatalf("colliding branch run names: %q", withSlash)
	}
}

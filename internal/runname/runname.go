// Package runname builds filesystem-safe parts for run directory names.
package runname

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode/utf8"
)

// maxReadableBytes leaves room in a 255-byte directory component for the
// repository, target prefix, hash, separators and timestamp.
const maxReadableBytes = 48

// BranchPart keeps a branch name recognizable while adding a hash of the
// original value. The hash prevents distinct names such as foo/bar and
// foo-bar from sharing a run directory after the readable part is flattened.
func BranchPart(branch string) string {
	readable := strings.ReplaceAll(branch, "/", "-")
	readable = strings.ReplaceAll(readable, "\\", "-")
	readable = strings.ReplaceAll(readable, "..", "-")
	if readable == "" {
		readable = "unknown"
	}
	if len(readable) > maxReadableBytes {
		end := maxReadableBytes
		for !utf8.RuneStart(readable[end]) {
			end--
		}
		readable = readable[:end]
	}
	sum := sha256.Sum256([]byte(branch))
	return fmt.Sprintf("%s-%x", readable, sum[:16])
}

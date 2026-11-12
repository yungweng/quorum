package rundir

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yungweng/quorum/internal/prkey"
)

// maxReadableBytes leaves room in a 255-byte directory component for the
// repository, target prefix, hash, separators and timestamp.
const maxReadableBytes = 48

// New returns the path of a fresh run directory under runsDir: the repository,
// the pull request or branch, and the start time to the second.
func New(runsDir, repo string, number int, branch string) string {
	if number == 0 {
		return named(runsDir, repo, "branch-"+branchPart(branch))
	}
	return ForPR(runsDir, repo, number)
}

// ForPR is New for a pull request number, whatever its value.
func ForPR(runsDir, repo string, number int) string {
	return named(runsDir, repo, fmt.Sprintf("pr-%d", number))
}

func named(runsDir, repo, target string) string {
	return filepath.Join(runsDir, fmt.Sprintf("%s-%s-%s",
		prkey.RepoSlug(repo), target, time.Now().Format("20060102-150405")))
}

// The hash of the original name prevents distinct names such as foo/bar and
// foo-bar from sharing a run directory after the readable part is flattened.
func branchPart(branch string) string {
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

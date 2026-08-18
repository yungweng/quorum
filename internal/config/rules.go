package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// RepoRules reads the user-local review rules for repo ("owner/repo") from
// dir, the rules root (~/.config/quorum/rules). The file existing is what
// activates the rules; a missing file simply means none. Any other read
// failure is returned: rules that exist but silently stop applying are the
// failure this file layout is meant to prevent.
//
// The rules live user-local rather than inside the target repository on
// purpose: a rules file in the repo would let the PR under review soften its
// own review.
func RepoRules(dir, repo string) (string, error) {
	if strings.Contains(repo, "..") {
		return "", nil
	}
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(repo)+".md"))
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

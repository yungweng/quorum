// Package prkey is the "owner/repo#number" identity every store and every
// dashboard row uses for a pull request.
package prkey

import (
	"fmt"
	"strconv"
	"strings"
)

// RepoSlug flattens owner/repo into one path component, "owner-repo". Run
// directories and the dependency cache both name a repository this way.
func RepoSlug(repo string) string {
	return strings.ReplaceAll(repo, "/", "-")
}

func Format(repo string, number int) string {
	return fmt.Sprintf("%s#%d", repo, number)
}

// Branch is the key of a run on a pushed branch that has no pull request. The
// history log and the live-run records both use it, which is what lets the
// dashboard tell that a logged branch run and a running one are the same.
func Branch(repo, branch string) string {
	return repo + "#branch:" + branch
}

// Repo returns the owner/repo half of a key, or the key itself when it has no
// number part.
func Repo(key string) string {
	repo, _, _ := strings.Cut(key, "#")
	return repo
}

// Number returns the pull request number, or 0 when the key does not name one,
// as a branch run's key does not.
func Number(key string) int {
	_, number, ok := strings.Cut(key, "#")
	if !ok {
		return 0
	}
	n, err := strconv.Atoi(number)
	if err != nil {
		return 0
	}
	return n
}

// Name returns just the repository name, which is what fits on a status line.
func Name(key string) string {
	repo := Repo(key)
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

func URL(repo string, number int) string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", repo, number)
}

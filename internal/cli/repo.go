package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/yungweng/quorum/internal/config"
)

// hereRepo resolves the repository the current working directory belongs to.
//
// `quorum review` and `quorum babysit` both work on the checkout you are
// standing in: they create their worktree from it, and its remote decides which
// repository the PR argument is read against.
func (a *app) hereRepo(t tools) (root, repo string, err error) {
	ctx := context.Background()
	wd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	root, err = a.newGit(t.Git).Toplevel(ctx, wd)
	if err != nil {
		return "", "", fmt.Errorf("run this from inside the target git repository")
	}
	repo, err = a.newGH(t.GH).CurrentRepo(ctx, root)
	if err != nil {
		return "", "", err
	}
	return root, repo, nil
}

var prURL = regexp.MustCompile(`^https://github\.com/([^/]+/[^/]+)/pull/(\d+)`)

// resolvePRArg reads a PR argument: a bare number, or a full GitHub PR URL.
// A bare number belongs to fallbackRepo, which is the checkout you are in.
//
// `quorum review` and `quorum babysit` use this because they always run inside
// the checkout. `quorum run` takes a repository it is told rather than one it
// stands in, so it uses resolvePR below.
func resolvePRArg(arg, fallbackRepo string) (int, string, error) {
	if m := prURL.FindStringSubmatch(arg); m != nil {
		n, _ := strconv.Atoi(m[2])
		return n, m[1], nil
	}
	if n, err := strconv.Atoi(strings.TrimPrefix(arg, "#")); err == nil {
		return n, fallbackRepo, nil
	}
	return 0, "", fmt.Errorf("expected a PR number or a GitHub PR URL like https://github.com/owner/repo/pull/123")
}

// resolvePR accepts a pull request URL, owner/repo#number, or a bare number
// when the working directory is inside a repository.
func (a *app) resolvePR(arg string) (string, int, error) {
	if m := prURL.FindStringSubmatch(arg); m != nil {
		n, _ := strconv.Atoi(m[2])
		return m[1], n, nil
	}
	if repo, num, ok := strings.Cut(arg, "#"); ok && strings.Contains(repo, "/") {
		n, err := strconv.Atoi(num)
		if err != nil {
			return "", 0, fmt.Errorf("%q does not end in a PR number", arg)
		}
		return repo, n, nil
	}
	if n, err := strconv.Atoi(arg); err == nil {
		gh, err := exec.LookPath("gh")
		if err != nil {
			return "", 0, fmt.Errorf("gh not found")
		}
		out, err := exec.Command(gh, "repo", "view", "--json", "nameWithOwner", "-q", ".nameWithOwner").Output()
		if err != nil {
			return "", 0, fmt.Errorf("not inside a repository, use owner/repo#%d", n)
		}
		return strings.TrimSpace(string(out)), n, nil
	}
	return "", 0, fmt.Errorf("cannot read %q as a pull request", arg)
}

// durationText renders a timeout the way the help and headers show it.
func durationText(d time.Duration) string {
	if d == 0 {
		return "off"
	}
	return config.FormatDuration(d)
}

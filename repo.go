package main

import (
	"context"
	"fmt"
	"os"
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

// resolvePRArg reads a PR argument: a bare number, or a full GitHub PR URL.
// A bare number belongs to fallbackRepo, which is the checkout you are in.
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

// durationText renders a timeout the way the help and headers show it.
func durationText(d time.Duration) string {
	if d == 0 {
		return "off"
	}
	return config.FormatDuration(d)
}

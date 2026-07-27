package review

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yungweng/quorum/internal/git"
)

func TestGCPreservesDependenciesForAResumedRun(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resumeRun string
		wantTree  bool
	}{
		{name: "resume", resumeRun: "retained-run", wantTree: true},
		{name: "fresh"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			runs := filepath.Join(root, "runs")
			depsRoot := filepath.Join(root, "deps")
			tree := filepath.Join(depsRoot, "owner-repo", "project", "hash")
			current := filepath.Join(runs, "current")
			worktree := filepath.Join(current, "worktree")
			if err := os.MkdirAll(worktree, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(tree, 0o755); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(tree, ".complete")
			if err := os.WriteFile(marker, nil, 0o644); err != nil {
				t.Fatal(err)
			}
			old := time.Now().Add(-depsRetention - time.Hour)
			if err := os.Chtimes(marker, old, old); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(worktree, "node_modules")
			if tc.resumeRun != "" {
				if err := os.Symlink(tree, link); err != nil {
					t.Fatal(err)
				}
			}

			runner := Runner{Git: git.G{Bin: "true"}}
			runner.gc(t.Context(), Options{
				RepoRoot:  root,
				RunsDir:   runs,
				DepsDir:   depsRoot,
				ResumeRun: tc.resumeRun,
			}, current)

			_, err := os.Stat(tree)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if got := err == nil; got != tc.wantTree {
				t.Errorf("dependency tree exists = %v, want %v", got, tc.wantTree)
			}
			if tc.resumeRun != "" {
				if _, err := os.Stat(link); err != nil {
					t.Errorf("retained dependency link is broken: %v", err)
				}
			}
		})
	}
}

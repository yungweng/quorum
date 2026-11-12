package engine

import (
	"strings"
	"testing"

	"github.com/yungweng/quorum/internal/claudecli"
	"github.com/yungweng/quorum/internal/codex"
	"github.com/yungweng/quorum/internal/engine/spec"
	"github.com/yungweng/quorum/internal/grokcli"
)

func TestNewReviewerDefaultsToCodex(t *testing.T) {
	r, err := NewReviewer(spec.Model{Name: "m"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	opts, ok := r.(codex.Options)
	if !ok {
		t.Fatalf("empty engine built %T, want codex.Options", r)
	}
	if !opts.DisableSerena {
		t.Error("the codex review engine must disable Serena")
	}
	if opts.Bypass {
		t.Error("a review engine carried the sandbox bypass")
	}
}

func TestNewReviewerBuildsClaude(t *testing.T) {
	r, err := NewReviewer(spec.Model{Engine: spec.Claude, Name: "sonnet"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	opts, ok := r.(claudecli.Options)
	if !ok {
		t.Fatalf("claude engine built %T", r)
	}
	if opts.Bypass {
		t.Error("a review engine carried the permission bypass")
	}
}

func TestNewReviewerBuildsGrok(t *testing.T) {
	r, err := NewReviewer(spec.Model{Engine: spec.Grok, Name: "grok-4.5"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	opts, ok := r.(grokcli.Options)
	if !ok {
		t.Fatalf("grok engine built %T", r)
	}
	if opts.Bypass {
		t.Error("a review engine carried the permission bypass")
	}
}

func TestNewReviewerRejectsUnknownEngine(t *testing.T) {
	_, err := NewReviewer(spec.Model{Engine: "gemini"}, nil)
	if err == nil || !strings.Contains(err.Error(), "gemini") {
		t.Fatalf("err = %v, want a rejection naming the engine", err)
	}
}

func TestNewFixerCarriesBypass(t *testing.T) {
	f, err := NewFixer(spec.Model{Engine: spec.Codex}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !f.(codex.Options).Bypass {
		t.Error("codex fixer lost the bypass")
	}
	cf, err := NewFixer(spec.Model{Engine: spec.Claude}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !cf.(claudecli.Options).Bypass {
		t.Error("claude fixer lost the bypass")
	}
	gf, err := NewFixer(spec.Model{Engine: spec.Grok}, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if !gf.(grokcli.Options).Bypass {
		t.Error("grok fixer lost the bypass")
	}
}

func TestNewFixerRejectsUnknownEngine(t *testing.T) {
	if _, err := NewFixer(spec.Model{Engine: "gemini"}, nil, false); err == nil {
		t.Fatal("unknown engine was accepted")
	}
}

// The binary each adapter is handed: the resolved one when there is one, and
// otherwise the engine's plain name from the spec table for PATH to find.
func TestConstructorsResolveTheBinary(t *testing.T) {
	bins := spec.Bins{spec.Codex: "/bin/codex", spec.Claude: "/bin/claude"}
	for _, tc := range []struct {
		eng  string
		bins spec.Bins
		want string
	}{
		{"", bins, "/bin/codex"},
		{spec.Claude, bins, "/bin/claude"},
		{spec.Grok, bins, "grok"},
		{"", nil, "codex"},
		{spec.Claude, nil, "claude"},
	} {
		r, err := NewReviewer(spec.Model{Engine: tc.eng}, tc.bins)
		if err != nil {
			t.Fatal(err)
		}
		f, err := NewFixer(spec.Model{Engine: tc.eng}, tc.bins, false)
		if err != nil {
			t.Fatal(err)
		}
		for _, got := range []string{bin(r), bin(f)} {
			if got != tc.want {
				t.Errorf("engine %q: binary %q, want %q", tc.eng, got, tc.want)
			}
		}
	}
}

func bin(v any) string {
	switch o := v.(type) {
	case codex.Options:
		return o.Bin
	case claudecli.Options:
		return o.Bin
	case grokcli.Options:
		return o.Bin
	}
	return ""
}

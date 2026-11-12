package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A value that cannot be used keeps the default, as before, but no longer in
// silence: AGENT_ACTION=babysitt used to review, and REVIEWERS=20 loaded and
// then failed every run.
func TestLoadReportsValuesItCannotUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	body := `AGENT_ACTION="babysitt"
REVIEWERS=20
MAX_CONCURRENT=0
NOTIFY=maybe
REVIEW_TIMEOUT=soon
LOAD_LIMIT=high
MAX_ITER=4
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err == nil {
		t.Fatal("Load accepted every value")
	}
	for _, want := range []string{
		`line 1: AGENT_ACTION="babysitt" ignored, expected review or babysit`,
		`line 2: REVIEWERS="20" ignored, expected 1 to 12`,
		`line 3: MAX_CONCURRENT="0" ignored, expected 1 or more`,
		`line 4: NOTIFY="maybe" ignored, expected 1 or 0`,
		`line 5: REVIEW_TIMEOUT="soon" ignored`,
		`line 6: LOAD_LIMIT="high" ignored`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("problems %q lack %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "MAX_ITER") {
		t.Errorf("a valid value was reported: %v", err)
	}
	def := Default()
	if cfg.AgentAction != def.AgentAction || cfg.Reviewers != def.Reviewers ||
		cfg.MaxConcurrent != def.MaxConcurrent || cfg.Notify != def.Notify ||
		cfg.ReviewTimeout != def.ReviewTimeout || cfg.LoadLimit != def.LoadLimit {
		t.Errorf("a rejected value replaced its default: %+v", cfg)
	}
	if cfg.MaxIter != 4 {
		t.Errorf("MaxIter = %d, the valid line after the bad ones was lost", cfg.MaxIter)
	}
}

// Zero switches a timeout off and an empty switch is off; neither is a typo.
func TestLoadAcceptsZeroAndEmptyValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	body := "REVIEW_TIMEOUT=0\nFIX_TIMEOUT=\"0\"\nNOTIFY=\nNICE=0\nLOAD_LIMIT=0\nMAX_RETRIES=0\n" +
		"REVIEW_ENGINE=\"\"\nREVIEWERS=\nAUTO_MERGE_TIMEOUT=\"\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReviewTimeout != 0 || cfg.FixTimeout != 0 || cfg.Notify || cfg.Nice != 0 || cfg.MaxRetries != 0 {
		t.Errorf("zero values were not applied: %+v", cfg)
	}
	def := Default()
	if cfg.ReviewEngine != def.ReviewEngine || cfg.Reviewers != def.Reviewers || cfg.AutoMergeTimeout != def.AutoMergeTimeout {
		t.Errorf("an empty value replaced its default: %+v", cfg)
	}
}

// What the file says about an unknown effort has to be what Load does.
func TestRenderedFileDescribesHowAnUnknownEffortIsHandled(t *testing.T) {
	text := Default().render()
	if strings.Contains(text, "refused") || !strings.Contains(text, "ignored") {
		t.Errorf("the rendered config still promises a refusal:\n%s", text)
	}
}

package harness

import (
	"context"
	"strings"
	"testing"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/usage"
	"github.com/anthropics/anthropic-sdk-go"
)

func TestHarnessUsageDisjointCacheAndEstimatedComponents(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	msg := &anthropic.Message{ID: "synthetic-request", Model: anthropic.Model("unknown-test-model"), Usage: anthropic.Usage{InputTokens: 100, OutputTokens: 30, CacheReadInputTokens: 150, CacheCreationInputTokens: 50}}
	e := recordUsageObservation(ctx, RunConfig{DataDir: dir}, "model", "session", 1, msg, strings.Repeat("x", 5000), "memory", []session.Message{{Content: "history"}}, "task")
	if e != nil {
		t.Fatal(e)
	}
	s, e := usage.Load(ctx, dir, false)
	if e != nil {
		t.Fatal(e)
	}
	if len(s.Events) != 1 || s.Events[0].Quantities["input_cache_read"] != 150 || *s.Contexts[0].UsedTokens != 330 {
		t.Fatalf("wrong cache accounting: %+v", s)
	}
	if len(s.Costs) != 0 {
		t.Fatal("unknown model has invented price")
	}
	if s.Contexts[0].LimitTokens != nil || s.Contexts[0].UsedPercent != nil {
		t.Fatal("unknown model capacity was guessed")
	}
	if s.Contexts[0].ComponentDelta == nil || *s.Contexts[0].ComponentDelta <= 0 {
		t.Fatal("heuristic mismatch hidden")
	}
}

func TestLegacyHarnessSummaryAlwaysHasPartialEstimateCoverage(t *testing.T) {
	db := tokenTestDB(t)
	defer db.Close()
	if e := RecordTokenUsage(db, "agent", "claude-sonnet-4", 100, 20, 0, 0); e != nil {
		t.Fatal(e)
	}
	s, e := LoadTokenUsageSummary(db, 30)
	if e != nil {
		t.Fatal(e)
	}
	if !s.Partial || s.Unpriced || s.CostKind != "estimate" || s.Source != "legacy-harness-rollup" {
		t.Fatalf("unqualified legacy spend: %+v", s)
	}
}

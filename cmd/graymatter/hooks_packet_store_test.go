package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

type fixedPacketEntity struct{}

func (fixedPacketEntity) ExtractIDs(string) ([]string, error) {
	return []string{"project:parcel"}, nil
}

func TestLexicalHookStore_GraphOverflowUsesWholeNativeResult(t *testing.T) {
	mem, err := graymatter.NewWithConfig(lexicalStoreConfig(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mem.Close() }()
	adv := mem.Advanced()
	ctx := context.Background()
	for i := 0; i < 32; i++ {
		if err := adv.Put(ctx, "parcel-project", fmt.Sprintf("Parcel dispatch reference %02d uses the loading entrance.", i)); err != nil {
			t.Fatal(err)
		}
	}
	graph, err := kg.Open(adv.DB())
	if err != nil {
		t.Fatal(err)
	}
	adapter := kg.NewGraphAdapter(graph)
	const neighbor = "Graph dispatch guidance requires a signed manifest."
	if err := adapter.UpsertNode("project:parcel", "Parcel project", "project"); err != nil {
		t.Fatal(err)
	}
	if err := adapter.UpsertNode("fact:manifest", neighbor, "fact"); err != nil {
		t.Fatal(err)
	}
	if err := adapter.LinkNodes("project:parcel", "fact:manifest", "requires"); err != nil {
		t.Fatal(err)
	}
	adv.SetKG(adapter, fixedPacketEntity{})
	store := &directStore{mem: mem, store: adv}
	var calls []int
	var actualNative []string
	got, receipt, err := hookRecallLexicalNamespace(ctx, func(ctx context.Context, k int) ([]string, error) {
		calls = append(calls, k)
		facts, err := store.Recall(ctx, "parcel-project", "parcel dispatch loading entrance", k)
		if k == 3 {
			actualNative = append([]string(nil), facts...)
		}
		return facts, err
	}, "parcel dispatch loading entrance", 3)
	if err == nil || !reflect.DeepEqual(calls, []int{32, 3}) {
		t.Fatalf("fallback calls=%v error=%v", calls, err)
	}
	if receipt.Candidates != 33 || receipt.Effective != "native" || receipt.Reason != "invalid_selection_input" {
		t.Fatalf("overflow receipt: %+v", receipt)
	}
	if len(actualNative) != 4 || !reflect.DeepEqual(got, actualNative) || got[3] != neighbor {
		t.Fatalf("native graph enrichment was truncated: got=%v native=%v", got, actualNative)
	}
	if receipt.Selected != 4 || receipt.Bytes != len(strings.Join(got, "\n\n")) {
		t.Fatalf("native receipt mismatches payload: %+v", receipt)
	}
}

func lexicalStoreConfig(dir string) graymatter.Config {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dir
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.ConsolidateLLM = ""
	cfg.AsyncConsolidate = false
	cfg.VectorReconcileInterval = 0
	cfg.DecayHalfLife = 720 * time.Hour
	return cfg
}

// Candidate retrieval uses the existing Recall accounting: every returned
// candidate is accessed, including candidates omitted from the final packet.
// Keep that effect visible because AccessedAt also controls later decay.
func TestLexicalHookStore_CandidateAccessAndDecay(t *testing.T) {
	cfg := lexicalStoreConfig(t.TempDir())
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = mem.Close()
		}
	})
	store := &directStore{mem: mem, store: mem.Advanced()}
	ctx := context.Background()
	const agent = "packet-project"
	for i := 0; i < 40; i++ {
		if err := store.Remember(ctx, agent, fmt.Sprintf("Parcel dispatch reference %02d uses the loading entrance.", i)); err != nil {
			t.Fatal(err)
		}
	}
	for _, text := range []string{"Shared parcel dispatch uses the signed manifest.", "Shared parcel dispatch needs a receipt."} {
		if err := store.PutShared(ctx, text); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Remember(ctx, "other-project", "Foreign parcel dispatch must never be injected."); err != nil {
		t.Fatal(err)
	}
	before, err := store.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 40 {
		t.Fatalf("seed facts = %d, want 40", len(before))
	}
	ageAnchor := time.Now().UTC().Add(-cfg.DecayHalfLife)
	byID := map[string]memory.Fact{}
	for _, fact := range before {
		fact.CreatedAt, fact.AccessedAt = ageAnchor, ageAnchor
		fact.AccessCount, fact.Weight = 0, 1
		if err := store.UpdateFact(agent, fact); err != nil {
			t.Fatal(err)
		}
		byID[fact.ID] = fact
	}
	started := time.Now().UTC()
	block, _, err := hookRecallLexicalBlock(ctx, store, agent, "parcel dispatch loading entrance")
	ended := time.Now().UTC()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, "Shared parcel dispatch") || strings.Contains(block, "Foreign parcel dispatch") {
		t.Fatalf("namespace separation failed: %q", block)
	}
	after, err := store.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	touched, visible := 0, 0
	omittedTouched := map[string]bool{}
	untouched := map[string]bool{}
	for _, fact := range after {
		original := byID[fact.ID]
		if fact.Text != original.Text || fact.CreatedAt != original.CreatedAt || fact.IsSuperseded() {
			t.Fatalf("recall changed content or lifecycle: %+v", fact)
		}
		if strings.Contains(block, fact.Text) {
			visible++
		}
		switch fact.AccessCount {
		case 1:
			touched++
			if fact.AccessedAt.Before(started) || fact.AccessedAt.After(ended) {
				t.Fatalf("access time outside recall: %v", fact.AccessedAt)
			}
			if !strings.Contains(block, fact.Text) {
				omittedTouched[fact.ID] = true
			}
		case 0:
			untouched[fact.ID] = true
			if !fact.AccessedAt.Equal(ageAnchor) {
				t.Fatalf("noncandidate was touched: %+v", fact)
			}
		default:
			t.Fatalf("access count = %d, want zero or one", fact.AccessCount)
		}
	}
	if touched != 32 || visible != 3 || len(omittedTouched) != 29 || len(untouched) != 8 {
		t.Fatalf("pool/packet accounting: touched=%d visible=%d omitted=%d untouched=%d", touched, visible, len(omittedTouched), len(untouched))
	}
	foreign, err := store.List("other-project")
	if err != nil || len(foreign) != 1 || foreign[0].AccessCount != 0 {
		t.Fatalf("foreign accesses: %v %v", foreign, err)
	}
	shared, err := store.List("__shared__")
	if err != nil || len(shared) != 2 {
		t.Fatalf("shared facts: %v %v", shared, err)
	}
	for _, fact := range shared {
		if fact.AccessCount != 1 {
			t.Fatalf("shared access count: %+v", fact)
		}
	}
	if err := store.Consolidate(ctx, agent); err != nil {
		t.Fatal(err)
	}
	decayed, err := store.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range decayed {
		if omittedTouched[fact.ID] && fact.Weight < 0.99 {
			t.Fatalf("accessed candidate decayed as stale: %+v", fact)
		}
		if untouched[fact.ID] && (fact.Weight < 0.49 || fact.Weight > 0.51) {
			t.Fatalf("untouched fact did not decay by one half-life: %+v", fact)
		}
	}
	if err := mem.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	reopened, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	persisted, err := reopened.Advanced().List(agent)
	if err != nil || len(persisted) != 40 {
		t.Fatalf("reopened facts: count=%d err=%v", len(persisted), err)
	}
	for _, fact := range persisted {
		if omittedTouched[fact.ID] && (fact.AccessCount != 1 || fact.Weight < 0.99) {
			t.Fatalf("candidate metadata did not persist: %+v", fact)
		}
	}
}

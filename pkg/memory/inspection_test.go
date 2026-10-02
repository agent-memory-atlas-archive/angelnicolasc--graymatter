package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func inspectionStore(t *testing.T, indexed bool) *Store {
	t.Helper()
	s, err := Open(StoreConfig{DataDir: t.TempDir(), CandidateRetrieval: indexed, UsageAliasLearning: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.now = func() time.Time { return time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC) }
	return s
}

func inspectionPut(t *testing.T, s *Store, agent, text string) Fact {
	t.Helper()
	f, err := s.PutReturningFact(context.Background(), agent, text)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func inspectionTxID(t *testing.T, s *Store) int {
	t.Helper()
	var id int
	if err := s.db.View(func(tx *bolt.Tx) error { id = tx.ID(); return nil }); err != nil {
		t.Fatal(err)
	}
	return id
}

type inspectionExtractor struct{ calls int }

func (e *inspectionExtractor) ExtractIDs(string) ([]string, error) {
	e.calls++
	return []string{"node"}, nil
}

func TestRecallPreviewDoesNotMutateOrFireHooks(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprint(indexed), func(t *testing.T) {
			s := inspectionStore(t, indexed)
			inspectionPut(t, s, "a", "PostgreSQL is our production database")
			inspectionPut(t, s, "a", "SQLite is the local cache")
			retired := inspectionPut(t, s, "retired", "old database")
			if _, err := s.CurateFact(context.Background(), "retired", retired.ID, "retire", "", retired); err != nil {
				t.Fatal(err)
			}
			var recalls, rankings int
			s.cfg.OnRecall = func(string, string, int, time.Duration) { recalls++ }
			s.debugRanking = func(string, []scored) { rankings++ }
			extractor := &inspectionExtractor{}
			s.SetKG(nil, extractor)
			before, _ := s.List("a")
			txID := inspectionTxID(t, s)
			got, err := s.RecallPreview(context.Background(), "a", "production database", 8)
			if err != nil || len(got) == 0 {
				t.Fatalf("preview: %+v %v", got, err)
			}
			for _, agent := range []string{"a", "missing", "retired"} {
				if _, err := s.RecallPreview(context.Background(), agent, "unknown vocabulary", 8); err != nil {
					t.Fatal(err)
				}
			}
			after, _ := s.List("a")
			if !reflect.DeepEqual(before, after) || inspectionTxID(t, s) != txID {
				t.Fatal("preview changed durable state")
			}
			if recalls != 0 || rankings != 0 || extractor.calls != 0 {
				t.Fatalf("preview called hooks: %d %d %d", recalls, rankings, extractor.calls)
			}
			s.SetKG(nil, nil)
			want, err := s.RecallExplain(context.Background(), "a", "production database", 8)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("ranking parity: preview=%+v explain=%+v err=%v", got, want, err)
			}
			if recalls != 1 || rankings != 1 {
				t.Fatal("ordinary recall hooks were disabled")
			}
		})
	}
}

func TestListFactsPaginationMetadataAndFilters(t *testing.T) {
	s := inspectionStore(t, true)
	ctx := context.Background()
	for i := 0; i < 7; i++ {
		inspectionPut(t, s, "a", fmt.Sprintf("Database fact %d", i))
	}
	alias, err := s.putReturningFactKind(ctx, "a", "db = database", KindAlias, "usage")
	if err != nil {
		t.Fatal(err)
	}
	retired := inspectionPut(t, s, "a", "old database")
	if _, err := s.CurateFact(ctx, "a", retired.ID, "retire", "", retired); err != nil {
		t.Fatal(err)
	}
	inspectionPut(t, s, "other", "Database fact elsewhere")
	txID := inspectionTxID(t, s)
	seen := map[string]bool{}
	cursor := ""
	for {
		page, err := s.ListFacts(ctx, "a", "active", "DATABASE", cursor, 3)
		if err != nil || page.Total != 7 || len(page.Facts) > 3 {
			t.Fatalf("page: %+v %v", page, err)
		}
		for _, f := range page.Facts {
			if seen[f.ID] || f.Embedding != nil || f.IsAlias() || f.IsSuperseded() {
				t.Fatalf("invalid row: %+v", f)
			}
			seen[f.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		if _, err := s.ListFacts(ctx, "other", "active", "DATABASE", page.NextCursor, 3); err == nil {
			t.Fatal("cross-namespace cursor accepted")
		}
		if _, err := s.ListFacts(ctx, "a", "all", "DATABASE", page.NextCursor, 3); err == nil {
			t.Fatal("cross-filter cursor accepted")
		}
		cursor = page.NextCursor
	}
	if len(seen) != 7 {
		t.Fatalf("visited %d rows", len(seen))
	}
	for state, total := range map[string]int{"all": 9, "retired": 1, "alias": 1} {
		page, err := s.ListFacts(ctx, "a", state, "", "", 100)
		if err != nil || page.Total != total {
			t.Fatalf("%s: %+v %v", state, page, err)
		}
		if state == "alias" && page.Facts[0].ID != alias.ID {
			t.Fatal("lost alias identity")
		}
	}
	if inspectionTxID(t, s) != txID {
		t.Fatal("paging wrote to database")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.ListFacts(cancelled, "a", "all", "", "", 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := s.ListFacts(ctx, "a", "invalid", "", "", 1); err == nil {
		t.Fatal("invalid state accepted")
	}
	// Unknown embedding fields are skipped; metadata remains suitable for a
	// precise optimistic-concurrency precondition without decoding a vector.
	raw := []byte(`{"id":"x","agent_id":"a","text":"row","embedding":{"not":"a vector"},"access_count":4,"pinned":true,"pinned_at":"2026-10-02T12:00:00Z"}`)
	f, err := unmarshalInspectionFact(raw)
	if err != nil || f.ID != "x" || f.AccessCount != 4 || !f.Pinned || f.PinnedAt.IsZero() || f.Embedding != nil {
		t.Fatalf("metadata: %+v %v", f, err)
	}
}

func TestCurateFactExactIdentityAndConflicts(t *testing.T) {
	s := inspectionStore(t, true)
	ctx := context.Background()
	original := inspectionPut(t, s, "a", "same text")
	sibling := inspectionPut(t, s, "a", "same text")
	if _, err := s.CurateFact(ctx, "wrong", original.ID, "retire", "", original); !errors.Is(err, ErrFactChanged) {
		t.Fatalf("wrong namespace: %v", err)
	}
	if _, err := s.CurateFact(ctx, "a", "missing", "pin", "", original); !errors.Is(err, ErrFactChanged) {
		t.Fatalf("wrong id: %v", err)
	}
	pinned, err := s.CurateFact(ctx, "a", original.ID, "pin", "", original)
	if err != nil || !pinned.Pinned || pinned.PinnedAt.IsZero() {
		t.Fatalf("pin: %+v %v", pinned, err)
	}
	txID := inspectionTxID(t, s)
	if _, err := s.CurateFact(ctx, "a", original.ID, "pin", "", pinned); err != nil {
		t.Fatal(err)
	}
	if inspectionTxID(t, s) != txID {
		t.Fatal("no-op pin wrote transaction")
	}
	if _, err := s.CurateFact(ctx, "a", original.ID, "revise", "stale replacement", original); !errors.Is(err, ErrFactChanged) {
		t.Fatalf("stale pin: %v", err)
	}
	// Recall/decay progress is not a content conflict and must be preserved.
	current := pinned
	current.AccessCount = 11
	current.Weight = .3
	if err := s.UpdateFact("a", current); err != nil {
		t.Fatal(err)
	}
	unpinned, err := s.CurateFact(ctx, "a", original.ID, "unpin", "", pinned)
	if err != nil || unpinned.AccessCount != 11 || unpinned.Weight != .3 || unpinned.Pinned {
		t.Fatalf("unpin overwrote current state: %+v %v", unpinned, err)
	}
	replacement, err := s.CurateFact(ctx, "a", original.ID, "revise", "new text", unpinned)
	if err != nil || replacement.ID == original.ID || replacement.Pinned || replacement.Confidence != "inferred" {
		t.Fatalf("revise: %+v %v", replacement, err)
	}
	all, _ := s.ListFacts(ctx, "a", "all", "", "", 100)
	if all.Total != 3 {
		t.Fatalf("unexpected writes: %+v", all)
	}
	for _, f := range all.Facts {
		if f.ID == original.ID && f.SupersededBy != replacement.ID {
			t.Fatal("lineage not committed")
		}
		if f.ID == sibling.ID && f.IsSuperseded() {
			t.Fatal("same-text sibling retired")
		}
	}
	if _, err := s.CurateFact(ctx, "a", original.ID, "retire", "", unpinned); !errors.Is(err, ErrFactChanged) {
		t.Fatalf("stale retirement: %v", err)
	}
	if _, err := s.CurateFact(ctx, "a", sibling.ID, "retire", "", sibling); err != nil {
		t.Fatal(err)
	}
	retired, _ := s.ListFacts(ctx, "a", "retired", "", "", 100)
	if retired.Total != 2 {
		t.Fatal("retirement deleted facts")
	}
}

func TestCurateAliasRetainsKindAndConservativeConfidence(t *testing.T) {
	s := inspectionStore(t, true)
	ctx := context.Background()
	alias, err := s.putReturningFactKind(ctx, "a", "db = database", KindAlias, "usage")
	if err != nil {
		t.Fatal(err)
	}
	alias.Confidence = "unverified"
	if err := s.UpdateFact("a", alias); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.CurateFact(ctx, "a", alias.ID, "revise", "db = datastore", alias)
	if err != nil || !replacement.IsAlias() || replacement.AliasSource != "" || replacement.Confidence != "unverified" {
		t.Fatalf("alias semantics: %+v %v", replacement, err)
	}
	receipts, err := s.RecallPreview(ctx, "a", "db", 8)
	if err != nil || len(receipts) != 0 {
		t.Fatalf("alias injected: %+v %v", receipts, err)
	}
}

func TestReadOnlyInspectionDoesNotCreateVectorArtifacts(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(StoreConfig{DataDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	f := inspectionPut(t, s, "a", "database")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a valid canonical store without a derived vector index by
	// retaining only its existing database in a separate inspection directory.
	readonlyDir := t.TempDir()
	raw, err := os.ReadFile(filepath.Join(dir, "gray.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(readonlyDir, "gray.db"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	s, err = Open(StoreConfig{DataDir: readonlyDir, ReadOnly: true, CandidateRetrieval: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.RecallPreview(context.Background(), "a", "database", 8); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CurateFact(context.Background(), "a", f.ID, "pin", "", f); !errors.Is(err, ErrStoreReadOnly) {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(readonlyDir)
	if err != nil || len(entries) != 1 || entries[0].Name() != "gray.db" {
		t.Fatalf("created artifacts: %v %v", entries, err)
	}
}

type inspectionCallbackEmbedder struct{ callback func() }

func (e inspectionCallbackEmbedder) Embed(context.Context, string) ([]float32, error) {
	e.callback()
	return nil, nil
}
func (inspectionCallbackEmbedder) Dimensions() int { return 2 }
func (inspectionCallbackEmbedder) Name() string    { return "inspection-fixture" }

func TestCurateFactRevalidatesAfterEmbeddingBeforeAtomicCommit(t *testing.T) {
	s := inspectionStore(t, true)
	ctx := context.Background()
	f := inspectionPut(t, s, "a", "database policy")
	puts := 0
	s.cfg.OnPut = func(string, string, time.Duration) { puts++ }
	s.embedder = inspectionCallbackEmbedder{callback: func() {
		if _, err := s.CurateFact(ctx, "a", f.ID, "pin", "", f); err != nil {
			t.Fatal(err)
		}
	}}
	replacement, err := s.CurateFact(ctx, "a", f.ID, "revise", "new policy", f)
	if !errors.Is(err, ErrFactChanged) || replacement.ID != "" {
		t.Fatalf("stale revision committed: %+v %v", replacement, err)
	}
	page, err := s.ListFacts(ctx, "a", "all", "", "", 10)
	if err != nil || page.Total != 1 || !page.Facts[0].Pinned || page.Facts[0].IsSuperseded() || puts != 0 {
		t.Fatalf("revision was not atomic: %+v %v puts=%d", page, err, puts)
	}
}

func TestInspectionHealthIsObservedNotAssumedReachability(t *testing.T) {
	s := inspectionStore(t, false)
	s.embedder = inspectionCallbackEmbedder{callback: func() { t.Fatal("health probed embedding provider") }}
	health, err := s.InspectHealth(context.Background())
	if err != nil || !health.ProviderConfigured || health.Provider != "inspection-fixture" || health.ProviderReachability != "not probed" {
		t.Fatalf("health: %+v %v", health, err)
	}
}

func TestPreviewDoesNotCreateMissingVectorCollection(t *testing.T) {
	s := inspectionStore(t, false)
	inspectionPut(t, s, "a", "database policy")
	s.embedder = &confidenceQueryEmbedder{}
	path := filepath.Join(s.cfg.DataDir, "vectors")
	before, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecallPreview(context.Background(), "a", "database", 8); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadDir(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != len(after) {
		t.Fatal("preview created a persistent vector collection")
	}
}

func TestListFactsCapsRequestedPageSize(t *testing.T) {
	s := inspectionStore(t, false)
	for i := 0; i < 503; i++ {
		inspectionPut(t, s, "a", fmt.Sprintf("fact %d", i))
	}
	page, err := s.ListFacts(context.Background(), "a", "all", "", "", 1000000)
	if err != nil || len(page.Facts) != 500 || page.Total != 503 || page.NextCursor == "" {
		t.Fatalf("unbounded page: size=%d total=%d err=%v", len(page.Facts), page.Total, err)
	}
}

package memory

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type confidenceQueryEmbedder struct {
	calls atomic.Int32
	err   error
}

func (e *confidenceQueryEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	e.calls.Add(1)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return []float32{1, 0}, e.err
}
func (e *confidenceQueryEmbedder) Dimensions() int { return 2 }
func (e *confidenceQueryEmbedder) Name() string    { return "confidence-fixture" }

type confidenceVectorBase struct{}

func (confidenceVectorBase) AddDocument(context.Context, string, string, string, []float32, map[string]string) error {
	return nil
}
func (confidenceVectorBase) EnsureCollection(string) error { return nil }
func (confidenceVectorBase) Close() error                  { return nil }
func (confidenceVectorBase) Query(context.Context, string, []float32, int) ([]VectorResult, error) {
	return nil, nil
}

type confidenceExhaustiveVectors struct {
	confidenceVectorBase
	results  []VectorResult
	requests []int
	cap      int
	err      error
	cancel   context.CancelFunc
}

func (v *confidenceExhaustiveVectors) QueryExhaustive(ctx context.Context, collection string, emb []float32, n int) ([]VectorResult, bool, error) {
	v.requests = append(v.requests, n)
	if v.err != nil {
		return nil, false, v.err
	}
	if v.cancel != nil {
		v.cancel()
		return nil, false, ctx.Err()
	}
	limit := min(n, len(v.results))
	if v.cap > 0 {
		limit = min(limit, v.cap)
	}
	return append([]VectorResult(nil), v.results[:limit]...), limit == len(v.results), nil
}

func TestConfidenceVectorExpansionAndExplicitExhaustion(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			s := confidenceRecallStore(t, indexed)
			var results []VectorResult
			results = append(results, VectorResult{ID: "orphan", Similarity: 1})
			for i := 0; i < 5; i++ {
				f := confidenceRecallSeed(t, s, "vectors", fmt.Sprintf("unchecked %d", i), "unverified", i)
				results = append(results, VectorResult{ID: f.ID, Similarity: 1 - float32(i+1)/20})
			}
			retired := confidenceRecallSeed(t, s, "vectors", "retired vector", "verified", 6)
			retired.SupersededBy = SupersededByAgent
			if err := s.UpdateFact("vectors", retired); err != nil {
				t.Fatal(err)
			}
			results = append(results, VectorResult{ID: retired.ID, Similarity: .6})
			alias := confidenceRecallSeed(t, s, "vectors", "neighborhood = clues", "verified", 7)
			alias.Kind = KindAlias
			if err := s.UpdateFact("vectors", alias); err != nil {
				t.Fatal(err)
			}
			results = append(results, VectorResult{ID: alias.ID, Similarity: .5})
			eligibleA := confidenceRecallSeed(t, s, "vectors", "first eligible vector", "verified", 8)
			eligibleB := confidenceRecallSeed(t, s, "vectors", "second eligible vector", "verified", 9)
			results = append(results, VectorResult{ID: eligibleA.ID, Similarity: .4}, VectorResult{ID: eligibleB.ID, Similarity: .3})
			backend := &confidenceExhaustiveVectors{results: results}
			emb := &confidenceQueryEmbedder{}
			s.vectors = backend
			s.embedder = emb
			minimum := "verified"
			s.cfg.SignalWeights = &SignalWeights{Vector: 1}
			r, err := s.RecallExplainWithOptions(context.Background(), "vectors", "lookup", 1, RecallOptions{MinConfidence: &minimum})
			if err != nil {
				t.Fatal(err)
			}
			if len(r.Receipts) != 1 || r.Receipts[0].Provenance.FactID != eligibleA.ID || r.Receipts[0].Ranks.VectorRank != 1 || r.Receipts[0].Ranks.FusedScore != 1.0/61 {
				t.Fatalf("eligible vector rank incorrect: %+v", r)
			}
			if !reflect.DeepEqual(backend.requests, []int{2, 4, 8, 16}) || emb.calls.Load() != 1 {
				t.Fatalf("expansion=%v embedding calls=%d", backend.requests, emb.calls.Load())
			}
			backend.requests = nil
			emb.calls.Store(0)
			r, err = s.RecallExplainWithOptions(context.Background(), "vectors", "lookup", 8, RecallOptions{MinConfidence: &minimum})
			if err != nil || len(r.Receipts) != 2 {
				t.Fatalf("exhausted eligible corpus: %+v %v", r, err)
			}
			if !reflect.DeepEqual(backend.requests, []int{16}) || emb.calls.Load() != 1 {
				t.Fatalf("exhaustion/embedding count: %v %d", backend.requests, emb.calls.Load())
			}
			stored, err := s.List("vectors")
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range stored {
				if f.ID != eligibleA.ID && f.ID != eligibleB.ID && f.AccessCount != 0 {
					t.Fatalf("discarded candidate was touched: %+v", f)
				}
			}
		})
	}
}

func TestConfidenceVectorIncompleteAndFailurePaths(t *testing.T) {
	s := confidenceRecallStore(t, true)
	f := confidenceRecallSeed(t, s, "vectors", "eligible result", "verified", 0)
	minimum := "verified"
	emb := &confidenceQueryEmbedder{}
	s.embedder = emb
	for _, tc := range []struct {
		name    string
		backend VectorStore
		ctx     context.Context
		want    string
	}{
		{"unsupported", confidenceVectorBase{}, context.Background(), "capability"},
		{"short_not_exhausted", &confidenceExhaustiveVectors{results: []VectorResult{{ID: "orphan", Similarity: 1}, {ID: f.ID, Similarity: .9}}, cap: 1}, context.Background(), "no progress"},
		{"backend_error", &confidenceExhaustiveVectors{err: errors.New("backend failure")}, context.Background(), "backend failure"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.vectors = tc.backend
			r, err := s.RecallWithOptions(tc.ctx, "vectors", "lookup", 8, RecallOptions{MinConfidence: &minimum})
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(r.Facts) != 0 {
				t.Fatalf("partial support accepted: %+v %v", r, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.vectors = &confidenceExhaustiveVectors{cancel: cancel}
	if _, err := s.RecallWithOptions(ctx, "vectors", "lookup", 8, RecallOptions{MinConfidence: &minimum}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation swallowed: %v", err)
	}
	emb.err = errors.New("embedding failure")
	s.vectors = confidenceVectorBase{}
	if _, err := s.RecallWithOptions(context.Background(), "vectors", "lookup", 8, RecallOptions{MinConfidence: &minimum}); err == nil || !strings.Contains(err.Error(), "embedding failure") {
		t.Fatalf("embedding error swallowed: %v", err)
	}
	stored, err := s.List("vectors")
	if err != nil || stored[0].AccessCount != 0 {
		t.Fatalf("failed recall touched candidate: %v %v", stored, err)
	}
}

func TestConfidenceNativeVectorFilteringAndKeywordProvider(t *testing.T) {
	s := confidenceRecallStore(t, true)
	var eligibleIDs []string
	for i := 0; i < 12; i++ {
		confidence := "unverified"
		if i >= 10 {
			confidence = "verified"
		}
		f := confidenceRecallSeed(t, s, "native", fmt.Sprintf("vector subject %d", i), confidence, i)
		if confidence == "verified" {
			eligibleIDs = append(eligibleIDs, f.ID)
		}
		if err := s.vectors.AddDocument(context.Background(), "native", f.ID, f.Text, []float32{1, 0}, nil); err != nil {
			t.Fatal(err)
		}
	}
	emb := &confidenceQueryEmbedder{}
	s.embedder = emb
	s.cfg.SignalWeights = &SignalWeights{Vector: 1}
	minimum := "verified"
	r, err := s.RecallExplainWithOptions(context.Background(), "native", "lookup", 8, RecallOptions{MinConfidence: &minimum})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Receipts) != 2 || r.Receipts[0].Provenance.FactID != eligibleIDs[0] || r.Receipts[1].Provenance.FactID != eligibleIDs[1] || emb.calls.Load() != 1 {
		t.Fatalf("native tie/order/filter: %+v count=%d", r, emb.calls.Load())
	}
	// The shipped keyword provider explicitly has zero dimensions and nil
	// embeddings. It must retain filtered keyword retrieval without a vector
	// capability or an embedding call.
	s.embedder = &confidenceKeywordEmbedder{}
	s.vectors = confidenceVectorBase{}
	r, err = s.RecallExplainWithOptions(context.Background(), "native", "lookup", 8, RecallOptions{MinConfidence: &minimum})
	if err != nil || len(r.Receipts) != 2 {
		t.Fatalf("keyword mode falsely requires vectors: %+v %v", r, err)
	}
}

func TestConfidenceNativeVectorPopulationSnapshotBlocksConcurrentUpsert(t *testing.T) {
	backend, err := newChromemVectorStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := backend.AddDocument(ctx, "population", "first", "first", []float32{1, 0}, nil); err != nil {
		t.Fatal(err)
	}
	eligible := map[string]bool{"first": true, "later": true}
	countRead := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	type queryResult struct {
		results   []VectorResult
		exhausted bool
		err       error
	}
	completed := make(chan queryResult, 1)
	go func() {
		results, exhausted, err := backend.queryEligible(ctx, "population", []float32{1, 0}, 8, eligible, func() {
			// This assertion is deterministic: the snapshot lock must remain
			// held between Count and QueryEmbedding, independently of whether
			// the concurrent writer has reached its lock yet.
			if backend.populationMu.TryLock() {
				backend.populationMu.Unlock()
				t.Error("Count and QueryEmbedding are not protected by one population snapshot")
			}
			close(countRead)
			<-release
		})
		completed <- queryResult{results, exhausted, err}
	}()
	select {
	case <-countRead:
	case <-time.After(5 * time.Second):
		t.Fatal("query did not reach population barrier")
	}
	writerStarted := make(chan struct{})
	written := make(chan error, 1)
	go func() {
		close(writerStarted)
		written <- backend.AddDocument(ctx, "population", "later", "later", []float32{0, 1}, nil)
	}()
	<-writerStarted
	unblock()
	select {
	case result := <-completed:
		if result.err != nil || !result.exhausted || len(result.results) != 1 || result.results[0].ID != "first" {
			t.Fatalf("query mixed population snapshots: %+v", result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("query did not finish after barrier release")
	}
	select {
	case err := <-written:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not finish after query snapshot")
	}
	results, exhausted, err := backend.QueryEligible(ctx, "population", []float32{1, 0}, 8, eligible)
	if err != nil || !exhausted || len(results) != 2 || results[0].ID != "first" || results[1].ID != "later" {
		t.Fatalf("next snapshot lost completed vector upsert: %+v, exhausted=%t, err=%v", results, exhausted, err)
	}
}

func TestConfidenceIndexedConcurrentDemotionKeepsFilterAndReceiptCoherent(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		name := "weighted"
		if filtered {
			name = "filtered"
		}
		t.Run(name, func(t *testing.T) {
			s := confidenceRecallStore(t, true)
			demoted := confidenceRecallSeed(t, s, "coherence", "routing guarded primary", "verified", 0)
			eligible := confidenceRecallSeed(t, s, "coherence", "routing eligible second", "verified", 1)
			for _, fact := range []Fact{demoted, eligible} {
				if err := s.vectors.AddDocument(context.Background(), "coherence", fact.ID, fact.Text, []float32{1, 0}, nil); err != nil {
					t.Fatal(err)
				}
			}
			embedder := &confidenceQueryEmbedder{}
			s.embedder = embedder
			var once sync.Once
			var rankings atomic.Int32
			s.debugRanking = func(string, []scored) {
				rankings.Add(1)
				once.Do(func() {
					demoted.Confidence = "unverified"
					if err := s.UpdateFact("coherence", demoted); err != nil {
						t.Fatal(err)
					}
				})
			}
			weight := .2
			opts := RecallOptions{ConfidenceWeight: &weight}
			if filtered {
				minimum := "verified"
				opts.MinConfidence = &minimum
			}
			result, err := s.RecallExplainWithOptions(context.Background(), "coherence", "routing", 8, opts)
			if err != nil || rankings.Load() != 2 || embedder.calls.Load() != 1 {
				t.Fatalf("guard did not use one scan fallback/embedding: %+v / %v; rankings=%d embeddings=%d", result, err, rankings.Load(), embedder.calls.Load())
			}
			if filtered {
				if len(result.Receipts) != 1 || result.Receipts[0].Provenance.FactID != eligible.ID {
					t.Fatalf("compact old confidence leaked through filter: %+v", result)
				}
			} else if len(result.Receipts) != 2 {
				t.Fatalf("weighted fallback lost candidates: %+v", result)
			}
			for _, receipt := range result.Receipts {
				factor := 1.2
				if receipt.Provenance.FactID == demoted.ID {
					factor = .8
					if receipt.Provenance.Confidence != "unverified" || receipt.Ranking.EffectiveConfidence != "unverified" {
						t.Fatalf("receipt retained obsolete label: %+v", receipt)
					}
				}
				if receipt.Ranking.Factor != factor || receipt.Ranking.FinalScore != receipt.Ranks.FusedScore*factor {
					t.Fatalf("receipt mixes compact and canonical confidence: %+v", receipt)
				}
			}
			stored, err := s.List("coherence")
			if err != nil {
				t.Fatal(err)
			}
			for _, fact := range stored {
				want := 1
				if filtered && fact.ID == demoted.ID {
					want = 0
				}
				if fact.AccessCount != want {
					t.Fatalf("discarded attempt touched a candidate: %+v; want %d", fact, want)
				}
			}
		})
	}
}

func TestConfidenceIndexedAliasChangeFailsBeforeSecondEmbedding(t *testing.T) {
	for _, filtered := range []bool{false, true} {
		t.Run(fmt.Sprintf("filtered=%t", filtered), func(t *testing.T) {
			s := confidenceRecallStore(t, true)
			fact := confidenceRecallSeed(t, s, "alias-coherence", "orchard routing reviewed", "verified", 0)
			alias, err := s.PutAlias(context.Background(), "alias-coherence", "lookup", []string{"orchard"})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.vectors.AddDocument(context.Background(), "alias-coherence", fact.ID, fact.Text, []float32{1, 0}, nil); err != nil {
				t.Fatal(err)
			}
			embedder := &confidenceQueryEmbedder{}
			s.embedder = embedder
			var once sync.Once
			s.debugRanking = func(string, []scored) {
				once.Do(func() {
					alias.Text = "alias: lookup = routing"
					if err := s.UpdateFact("alias-coherence", alias); err != nil {
						t.Fatal(err)
					}
				})
			}
			weight := .2
			opts := RecallOptions{ConfidenceWeight: &weight}
			if filtered {
				minimum := "verified"
				opts.MinConfidence = &minimum
			}
			result, err := s.RecallExplainWithOptions(context.Background(), "alias-coherence", "lookup", 8, opts)
			if !errors.Is(err, errConfidenceQueryChanged) || len(result.Receipts) != 0 || embedder.calls.Load() != 1 {
				t.Fatalf("concurrent alias edit silently changed query or embedded twice: %+v / %v; embeddings=%d", result, err, embedder.calls.Load())
			}
			stored, err := s.List("alias-coherence")
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range stored {
				if f.AccessCount != 0 {
					t.Fatalf("failed indexed attempt touched a fact: %+v", f)
				}
			}
		})
	}
}

type confidenceKeywordEmbedder struct{}

func (*confidenceKeywordEmbedder) Embed(context.Context, string) ([]float32, error) {
	panic("zero-dimensional provider must not be asked to embed filtered query")
}
func (*confidenceKeywordEmbedder) Dimensions() int { return 0 }
func (*confidenceKeywordEmbedder) Name() string    { return "keyword-fixture" }

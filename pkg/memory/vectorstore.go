package memory

import (
	"context"
	"math"
	"path/filepath"
	"sort"
	"sync"

	chromem "github.com/philippgille/chromem-go"
)

// VectorResult is a single result from a vector similarity query.
type VectorResult struct {
	ID         string
	Content    string
	Similarity float32
}

// VectorStore is the pluggable vector search backend.
// The default implementation wraps chromem-go (zero-infra, pure-Go, persistent).
// Future implementations can wrap Qdrant, Weaviate, pgvector, etc.
//
// Implementations must be safe for concurrent use.
type VectorStore interface {
	// AddDocument upserts a document with its pre-computed embedding.
	AddDocument(ctx context.Context, collection, id, content string, embedding []float32, metadata map[string]string) error

	// Query returns at most n documents ranked by cosine similarity to embedding.
	Query(ctx context.Context, collection string, embedding []float32, n int) ([]VectorResult, error)

	// EnsureCollection creates the collection if it does not exist.
	EnsureCollection(collection string) error

	// Close flushes and releases any resources held by the store.
	Close() error
}

// EligibleVectorStore optionally ranks only supplied canonical live IDs. When
// fewer than n unique eligible results are returned, exhausted must explicitly
// confirm there are no more matches. It must retain similarity/ID ordering and
// be safe for concurrent calls, like VectorStore.
type EligibleVectorStore interface {
	QueryEligible(ctx context.Context, collection string, embedding []float32, n int, eligible map[string]bool) (results []VectorResult, exhausted bool, err error)
}

// ExhaustiveVectorStore optionally exposes a complete ordered prefix and an
// explicit exhaustion signal. Increasing n must extend the same similarity/ID
// ordered prefix, not sample or cap it silently. Filtered retrieval reuses one
// embedding while expanding this prefix until its eligible budget is met.
type ExhaustiveVectorStore interface {
	QueryExhaustive(ctx context.Context, collection string, embedding []float32, n int) (results []VectorResult, exhausted bool, err error)
}

func orderedEligibleVectors(results []VectorResult, eligible map[string]bool) []VectorResult {
	seen := make(map[string]VectorResult, len(results))
	for _, r := range results {
		if !eligible[r.ID] || math.IsNaN(float64(r.Similarity)) || math.IsInf(float64(r.Similarity), 0) {
			continue
		}
		if old, exists := seen[r.ID]; !exists || r.Similarity > old.Similarity {
			seen[r.ID] = r
		}
	}
	out := make([]VectorResult, 0, len(seen))
	for _, r := range seen {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Similarity != out[j].Similarity {
			return out[i].Similarity > out[j].Similarity
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// chromemVectorStore wraps chromem-go to satisfy VectorStore.
//
// collections is a plain map behind mu. The interface contract above promises
// concurrent safety and the store delivers on it: Put calls addToVector
// outside the store mutex, the daemon serves every RPC in its own goroutine,
// and the background reconcile loop touches the same map. Without mu, two
// agents writing at once hit "concurrent map read and map write", which is a
// fatal error no recover() can catch — the daemon dies and takes every
// client's memory with it.
type chromemVectorStore struct {
	db          *chromem.DB
	mu          sync.Mutex
	collections map[string]*chromem.Collection
	// Native filtered exhaustion depends on Count and QueryEmbedding seeing
	// one document population. Every vector upsert, including reconciliation,
	// enters here; concurrent filtered queries share the read lock.
	populationMu sync.RWMutex
}

// newChromemVectorStore opens or creates a persistent chromem-go DB at dataDir/vectors.
func newChromemVectorStore(dataDir string) (*chromemVectorStore, error) {
	vecDir := filepath.Join(dataDir, "vectors")
	db, err := chromem.NewPersistentDB(vecDir, false)
	if err != nil {
		return nil, err
	}
	return &chromemVectorStore{
		db:          db,
		collections: make(map[string]*chromem.Collection),
	}, nil
}

func (c *chromemVectorStore) EnsureCollection(name string) error {
	_, err := c.collection(name)
	return err
}

// collection returns the named collection, creating it on first use. The lock
// is held across GetOrCreateCollection so two callers racing on a new
// collection agree on which handle wins.
func (c *chromemVectorStore) collection(name string) (*chromem.Collection, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if col, ok := c.collections[name]; ok {
		return col, nil
	}
	col, err := c.db.GetOrCreateCollection(name, nil, nil)
	if err != nil {
		return nil, err
	}
	c.collections[name] = col
	return col, nil
}

func (c *chromemVectorStore) AddDocument(ctx context.Context, collection, id, content string, embedding []float32, metadata map[string]string) error {
	col, err := c.collection(collection)
	if err != nil {
		return err
	}
	c.populationMu.Lock()
	defer c.populationMu.Unlock()
	return col.AddDocument(ctx, chromem.Document{
		ID:        id,
		Content:   content,
		Metadata:  metadata,
		Embedding: embedding,
	})
}

func (c *chromemVectorStore) Query(ctx context.Context, collection string, embedding []float32, n int) ([]VectorResult, error) {
	col, err := c.collection(collection)
	if err != nil {
		return nil, err
	}
	count := col.Count()
	if count == 0 || n <= 0 {
		return nil, nil
	}
	// chromem-go v0.7.0 refuses nResults greater than the number of documents
	// (an error instead of "at most n"), which silently killed the vector
	// signal on young stores: a recall over three facts asks for 2*topK=16
	// results and got an error the pipeline swallows by design. And with tied
	// similarities chromem's top-n selection follows its internal map order,
	// so even WHICH documents came back differed call to call.
	//
	// QueryEmbedding scans every document regardless of n, so fetching all
	// and selecting here costs the same. Selection is the total order the
	// recall pipeline imposes everywhere else — similarity descending, then
	// ID ascending — truncated to n: identical to chromem's pick when
	// similarities are distinct, deterministic when they tie.
	raw, err := col.QueryEmbedding(ctx, embedding, count, nil, nil)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(raw, func(i, j int) bool {
		if raw[i].Similarity != raw[j].Similarity {
			return raw[i].Similarity > raw[j].Similarity
		}
		return raw[i].ID < raw[j].ID
	})
	if len(raw) > n {
		raw = raw[:n]
	}
	results := make([]VectorResult, len(raw))
	for i, r := range raw {
		results[i] = VectorResult{
			ID:         r.ID,
			Content:    r.Content,
			Similarity: r.Similarity,
		}
	}
	return results, nil
}

// QueryEligible scans and orders the native population once, then intersects
// canonical eligibility before truncation. Vector documents may outlive their
// canonical facts; aliases, tombstones and orphan IDs cannot occupy the budget.
func (c *chromemVectorStore) QueryEligible(ctx context.Context, collection string, embedding []float32, n int, eligible map[string]bool) ([]VectorResult, bool, error) {
	return c.queryEligible(ctx, collection, embedding, n, eligible, nil)
}

// afterCount is a deterministic test seam inside the same population snapshot
// used by the public capability; production calls leave it nil.
func (c *chromemVectorStore) queryEligible(ctx context.Context, collection string, embedding []float32, n int, eligible map[string]bool, afterCount func()) ([]VectorResult, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	col, err := c.collection(collection)
	if err != nil {
		return nil, false, err
	}
	c.populationMu.RLock()
	defer c.populationMu.RUnlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	count := col.Count()
	if afterCount != nil {
		afterCount()
	}
	if count == 0 || n <= 0 {
		return nil, true, nil
	}
	raw, err := col.QueryEmbedding(ctx, embedding, count, nil, nil)
	if err != nil {
		return nil, false, err
	}
	results := make([]VectorResult, 0, len(raw))
	for _, r := range raw {
		results = append(results, VectorResult{ID: r.ID, Content: r.Content, Similarity: r.Similarity})
	}
	if err := validateConfidenceVectors(results); err != nil {
		return nil, false, err
	}
	results = orderedEligibleVectors(results, eligible)
	exhausted := len(results) <= n
	if len(results) > n {
		results = results[:n]
	}
	return results, exhausted, ctx.Err()
}

func (c *chromemVectorStore) Close() error {
	// chromem-go does not require an explicit close; data is flushed on write.
	return nil
}

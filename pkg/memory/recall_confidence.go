package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// recallPolicy is immutable request state. It owns copied option values so a
// request never changes the store configuration or another query's policy.
type recallPolicy struct {
	minimum   *string
	weight    float64
	metadata  bool
	embedding *confidenceEmbeddingCache
}

// A single pipeline may fall back from the compact index to a canonical scan
// after an overlapping mutation. It reuses the same embedding, never embeds
// twice or silently switches an alias-expanded query within one request.
type confidenceEmbeddingCache struct {
	loaded bool
	query  string
	value  []float32
	err    error
}

var errConfidenceQueryChanged = errors.New("confidence recall effective query changed during retrieval; retry the query")

func (s *Store) embedConfidenceQuery(ctx context.Context, query string, p recallPolicy) ([]float32, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.embedder == nil || s.embedder.Dimensions() == 0 {
		return nil, nil
	}
	if p.embedding == nil {
		return s.embedder.Embed(ctx, query)
	}
	if p.embedding.loaded {
		if p.embedding.query != query {
			return nil, errConfidenceQueryChanged
		}
		return p.embedding.value, p.embedding.err
	}
	p.embedding.loaded, p.embedding.query = true, query
	p.embedding.value, p.embedding.err = s.embedder.Embed(ctx, query)
	return p.embedding.value, p.embedding.err
}

// DefaultConfidenceWeight exposes the configured request default to optional
// adapters so direct and negotiated RPC calls resolve the same policy once.
func (s *Store) DefaultConfidenceWeight() float64 { return s.cfg.ConfidenceWeight }

func (s *Store) resolveRecallPolicy(opts RecallOptions) (recallPolicy, error) {
	if err := opts.Validate(); err != nil {
		return recallPolicy{}, err
	}
	p := recallPolicy{weight: s.cfg.ConfidenceWeight, metadata: opts.Requested()}
	if opts.ConfidenceWeight != nil {
		p.weight = *opts.ConfidenceWeight
	}
	if opts.MinConfidence != nil {
		v := *opts.MinConfidence
		p.minimum = &v
	}
	if p.weight != 0 {
		p.metadata = true
	}
	return p, nil
}

func (p recallPolicy) filtered() bool { return p.minimum != nil }
func (p recallPolicy) eligible(raw string) bool {
	return p.minimum == nil || ConfidenceLevel(raw) >= ConfidenceLevel(*p.minimum)
}
func (p recallPolicy) factor(raw string) float64 {
	return 1 + p.weight*float64(ConfidenceLevel(raw)-1)
}
func (p recallPolicy) retrieval() *RetrievalMetadata {
	if !p.metadata {
		return nil
	}
	kg := "hints_without_confidence_receipts"
	if p.filtered() {
		kg = "suppressed_min_confidence"
	}
	return &RetrievalMetadata{MinConfidence: p.minimum, ConfidenceWeight: p.weight, Policy: ConfidencePolicy, KG: kg}
}

func recallTopK(topK int) int {
	if topK <= 0 {
		return 8
	}
	return topK
}

func vectorBudget(topK int) int {
	maxInt := int(^uint(0) >> 1)
	if topK > maxInt/2 {
		return maxInt
	}
	return topK * 2
}

// RecallWithOptions applies confidence filtering before every retrieval signal
// and a bounded preference to the fused score. Metadata is also returned when
// the eligible corpus is empty.
func (s *Store) RecallWithOptions(ctx context.Context, agentID, query string, topK int, opts RecallOptions) (RecallResult, error) {
	p, err := s.resolveRecallPolicy(opts)
	if err != nil {
		return RecallResult{}, err
	}
	facts, feedback, err := s.recallDetailedWithPolicy(ctx, agentID, query, recallTopK(topK), p)
	return RecallResult{Facts: facts, Feedback: feedback, Retrieval: p.retrieval()}, err
}

// RecallExplainWithOptions reads out the same ranking that selects plain facts.
func (s *Store) RecallExplainWithOptions(ctx context.Context, agentID, query string, topK int, opts RecallOptions) (RecallExplainResult, error) {
	p, err := s.resolveRecallPolicy(opts)
	if err != nil {
		return RecallExplainResult{}, err
	}
	receipts, err := s.recallExplainWithPolicy(ctx, agentID, query, recallTopK(topK), p)
	return RecallExplainResult{Receipts: receipts, Retrieval: p.retrieval()}, err
}

func (s *Store) RecallSharedWithOptions(ctx context.Context, query string, topK int, opts RecallOptions) (RecallResult, error) {
	return s.RecallWithOptions(ctx, SharedAgentID, query, topK, opts)
}

// RecallAllWithOptions preserves the namespace RRF merge policy after applying
// exactly one effective policy to each input ranking.
func (s *Store) RecallAllWithOptions(ctx context.Context, agentID, query string, topK int, opts RecallOptions) (RecallResult, error) {
	p, err := s.resolveRecallPolicy(opts)
	if err != nil {
		return RecallResult{}, err
	}
	return s.recallAllWithPolicy(ctx, agentID, query, recallTopK(topK), p)
}

// Both public All methods share collection and bookkeeping. The older method
// supplies its original raw topK semantics; option-aware calls normalize it.
func (s *Store) recallAllWithPolicy(ctx context.Context, agentID, query string, topK int, p recallPolicy) (RecallResult, error) {
	agent, af, agentSelected, err := s.recallDetailedCollectWithPolicy(ctx, agentID, query, topK, p, false)
	if err != nil {
		return RecallResult{}, fmt.Errorf("recall agent: %w", err)
	}
	shared, sf, sharedSelected, err := s.recallDetailedCollectWithPolicy(ctx, SharedAgentID, query, topK, p, false)
	if err != nil {
		return RecallResult{}, fmt.Errorf("recall shared: %w", err)
	}
	feedback := af
	if sf != "" && sf != af {
		if feedback != "" {
			feedback += "\n"
		}
		feedback += sf
	}
	facts := fuseRecallResults(agent, shared, topK)
	selectedByText := make(map[string]Fact, len(agentSelected)+len(sharedSelected))
	// Namespace fusion gives agent facts the stable preference on a text
	// duplicate. Bookkeeping follows that same canonical identity rather than
	// touching every duplicate that contributed to the merged ranking.
	for _, f := range agentSelected {
		selectedByText[f.Text] = f
	}
	for _, f := range sharedSelected {
		if _, exists := selectedByText[f.Text]; !exists {
			selectedByText[f.Text] = f
		}
	}
	touched := make([]Fact, 0, len(facts))
	for _, text := range facts {
		if f, exists := selectedByText[text]; exists {
			touched = append(touched, f)
		}
	}
	s.touchFacts(touched)
	return RecallResult{Facts: facts, Feedback: feedback, Retrieval: p.retrieval()}, nil
}

// vectorSearchWithPolicy preserves the original backend request when no filter
// is present. A filtered request needs an explicit completeness capability;
// VectorStore's at-most-n contract cannot establish exhaustion.
func (s *Store) vectorSearchWithPolicy(ctx context.Context, agentID, query string, n int, eligible map[string]int, p recallPolicy) ([]VectorResult, error) {
	if !p.filtered() {
		if p.embedding == nil {
			return s.vectorSearch(ctx, agentID, query, n)
		}
		embedding, err := s.embedConfidenceQuery(ctx, query, p)
		if err != nil || len(embedding) == 0 {
			return nil, err
		}
		return s.vectors.Query(ctx, agentID, embedding, n)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.embedder == nil || s.embedder.Dimensions() == 0 {
		return nil, nil
	}
	emb, err := s.embedConfidenceQuery(ctx, query, p)
	if err != nil {
		return nil, fmt.Errorf("filtered vector embedding: %w", err)
	}
	if len(emb) == 0 {
		return nil, fmt.Errorf("filtered vector embedding is empty")
	}
	ids := make(map[string]bool, len(eligible))
	for id := range eligible {
		ids[id] = true
	}
	if backend, ok := s.vectors.(EligibleVectorStore); ok {
		results, exhausted, err := backend.QueryEligible(ctx, agentID, emb, n, ids)
		if err != nil {
			return nil, err
		}
		if err := validateConfidenceVectors(results); err != nil {
			return nil, err
		}
		results = orderedEligibleVectors(results, ids)
		if len(results) < n && !exhausted {
			return nil, fmt.Errorf("filtered vector query incomplete: exhaustion was not established")
		}
		if len(results) > n {
			results = results[:n]
		}
		return results, ctx.Err()
	}
	backend, ok := s.vectors.(ExhaustiveVectorStore)
	if !ok {
		return nil, fmt.Errorf("filtered vector retrieval requires eligible-query or explicit-exhaustion capability")
	}
	requested := n
	seen := make(map[string]VectorResult)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		results, exhausted, err := backend.QueryExhaustive(ctx, agentID, emb, requested)
		if err != nil {
			return nil, err
		}
		progress := false
		prefix := make(map[string]bool, len(results))
		for _, r := range results {
			if math.IsNaN(float64(r.Similarity)) || math.IsInf(float64(r.Similarity), 0) {
				return nil, fmt.Errorf("filtered vector backend returned non-finite similarity")
			}
			if _, exists := seen[r.ID]; !exists {
				progress = true
				seen[r.ID] = r
			} else if seen[r.ID].Similarity != r.Similarity {
				return nil, fmt.Errorf("filtered vector query incomplete: backend changed an established prefix similarity")
			}
			prefix[r.ID] = true
		}
		for id := range seen {
			if !prefix[id] {
				return nil, fmt.Errorf("filtered vector query incomplete: backend did not extend its established prefix")
			}
		}
		all := make([]VectorResult, 0, len(seen))
		for _, r := range seen {
			all = append(all, r)
		}
		all = orderedEligibleVectors(all, ids)
		if len(all) >= n {
			return all[:n], nil
		}
		if exhausted {
			return all, nil
		}
		if !progress {
			return nil, fmt.Errorf("filtered vector query incomplete: backend made no progress without exhaustion")
		}
		maxInt := int(^uint(0) >> 1)
		if requested >= maxInt {
			return nil, fmt.Errorf("filtered vector query incomplete: neighbor budget overflow")
		}
		if requested > maxInt/2 {
			requested = maxInt
		} else {
			requested *= 2
		}
	}
}

func validateConfidenceVectors(results []VectorResult) error {
	for _, r := range results {
		if math.IsNaN(float64(r.Similarity)) || math.IsInf(float64(r.Similarity), 0) {
			return fmt.Errorf("filtered vector backend returned non-finite similarity")
		}
	}
	return nil
}

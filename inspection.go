package graymatter

import (
	"context"
	"fmt"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// RecallPreview inspects retrieval without recording a recall or learning from
// the query. It uses the same configured top-k and ranking policy as Recall.
func (m *Memory) RecallPreview(ctx context.Context, agent, query string) ([]memory.RecallReceipt, error) {
	if m.store == nil {
		return nil, fmt.Errorf("graymatter: inspection unavailable: %w", m.initErr)
	}
	return m.store.RecallPreview(ctx, agent, query, m.cfg.TopK)
}

// ListFacts returns a bounded, embedding-free inspection page.
func (m *Memory) ListFacts(ctx context.Context, agent, state, query, cursor string, limit int) (memory.FactPage, error) {
	if m.store == nil {
		return memory.FactPage{}, fmt.Errorf("graymatter: inspection unavailable: %w", m.initErr)
	}
	return m.store.ListFacts(ctx, agent, state, query, cursor, limit)
}

// CurateFact applies one optimistic mutation to an exact selected identity.
func (m *Memory) CurateFact(ctx context.Context, agent, id, action, text string, expected memory.Fact) (memory.Fact, error) {
	if m.store == nil {
		return memory.Fact{}, fmt.Errorf("graymatter: curation unavailable: %w", m.initErr)
	}
	return m.store.CurateFact(ctx, agent, id, action, text, expected)
}

package server

import (
	"context"
	"errors"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// The REST application retains its established ranking independently of the
// product confidence rollout. Old daemons already serve the legacy endpoint.
func (s *Server) legacyRecall(ctx context.Context, agentID, query string, topK int) ([]string, error) {
	if backend, ok := s.store.(interface {
		RecallWithOptions(context.Context, string, string, int, memory.RecallOptions) (memory.RecallResult, error)
	}); ok {
		zero := 0.0
		result, err := backend.RecallWithOptions(ctx, agentID, query, topK, memory.RecallOptions{ConfidenceWeight: &zero})
		if !errors.Is(err, memory.ErrConfidenceUnsupported) {
			return result.Facts, err
		}
	}
	return s.store.Recall(ctx, agentID, query, topK)
}

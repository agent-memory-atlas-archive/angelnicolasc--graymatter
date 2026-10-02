package graymatter_test

import (
	"context"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	bolt "go.etcd.io/bbolt"
)

// This consumer implements the AdvancedStore method set from bb719714, without
// embedding the current interface or concrete Store. Interface embedding could
// conceal a source-incompatible new mandatory method.
type legacyAdvancedStore struct{}

func (legacyAdvancedStore) Put(context.Context, string, string) error { return nil }
func (legacyAdvancedStore) PutShared(context.Context, string) error   { return nil }
func (legacyAdvancedStore) Recall(context.Context, string, string, int) ([]string, error) {
	return nil, nil
}
func (legacyAdvancedStore) RecallShared(context.Context, string, int) ([]string, error) {
	return nil, nil
}
func (legacyAdvancedStore) List(string) ([]memory.Fact, error) { return nil, nil }
func (legacyAdvancedStore) ListAgents() ([]string, error)      { return nil, nil }
func (legacyAdvancedStore) Stats(string) (memory.MemoryStats, error) {
	return memory.MemoryStats{}, nil
}
func (legacyAdvancedStore) Delete(string, string) error          { return nil }
func (legacyAdvancedStore) UpdateFact(string, memory.Fact) error { return nil }
func (legacyAdvancedStore) Consolidate(context.Context, string, memory.ConsolidateConfig) error {
	return nil
}
func (legacyAdvancedStore) PendingVectorCount() int                                    { return 0 }
func (legacyAdvancedStore) DB() *bolt.DB                                               { return nil }
func (legacyAdvancedStore) SetKG(memory.GraphAccessor, memory.EntityExtractorAccessor) {}
func (legacyAdvancedStore) PutConfident(context.Context, string, string, string) error { return nil }
func (legacyAdvancedStore) IsReadOnly() bool                                           { return false }

var (
	_ graymatter.AdvancedStore                                                             = legacyAdvancedStore{}
	_ func(*graymatter.Memory, context.Context, string, string) error                      = (*graymatter.Memory).Remember
	_ func(*graymatter.Memory, context.Context, string, string) ([]string, error)          = (*graymatter.Memory).Recall
	_ func(*memory.Store, context.Context, string, string) error                           = (*memory.Store).Put
	_ func(*memory.Store, context.Context, string, string, string) error                   = (*memory.Store).PutConfident
	_ func(*memory.Store, context.Context, string, string, int) ([]string, error)          = (*memory.Store).Recall
	_ func(*memory.Store, context.Context, string, string, ...memory.Fact) (string, error) = (*memory.Store).Revise
)

func TestLegacyAdvancedStoreConsumerRequiresNoConfidenceMethods(t *testing.T) {
	var advanced graymatter.AdvancedStore = legacyAdvancedStore{}
	if err := advanced.Put(context.Background(), "legacy-consumer", "source"); err != nil {
		t.Fatal(err)
	}
	if _, ok := advanced.(interface {
		PutWithOptionsReturningFact(context.Context, string, string, memory.WriteOptions) (memory.Fact, error)
	}); ok {
		t.Fatal("legacy consumer accidentally implements new optional capability")
	}
}

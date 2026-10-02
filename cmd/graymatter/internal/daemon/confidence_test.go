package daemon

import (
	"context"
	"errors"
	"reflect"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

type confidenceAsyncBackend struct {
	graymatter.AdvancedStore
	rpc.ConfidenceWriter
	launches int
	observed []memory.Fact
}

func (b *confidenceAsyncBackend) LaunchAsyncConsolidate(a string, cfg memory.ConsolidateConfig) {
	b.launches++
	b.observed, _ = b.AdvancedStore.List(a)
}

func (b *confidenceAsyncBackend) PutSharedWithOptionsReturningFact(ctx context.Context, text string, opts memory.WriteOptions) (memory.Fact, error) {
	return b.AdvancedStore.(rpc.SharedConfidenceWriter).PutSharedWithOptionsReturningFact(ctx, text, opts)
}

func TestConfidenceBackendConsolidatesAfterMetadataCommit(t *testing.T) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.AsyncConsolidate = true
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	advanced := mem.Advanced()
	fake := &confidenceAsyncBackend{AdvancedStore: advanced, ConfidenceWriter: advanced.(rpc.ConfidenceWriter)}
	backend := rememberBackend{AdvancedStore: fake, mem: mem}
	label := "verified"
	fact, err := backend.PutWithOptionsReturningFact(context.Background(), "a", "database policy", memory.WriteOptions{Confidence: &label})
	if err != nil || fake.launches != 1 || len(fake.observed) != 1 || fake.observed[0].ID != fact.ID || fake.observed[0].Confidence != label {
		t.Fatalf("metadata/trigger mismatch: fact=%+v err=%v launches=%d observed=%+v", fact, err, fake.launches, fake.observed)
	}
	shared, err := backend.PutSharedWithOptionsReturningFact(context.Background(), "shared database policy", memory.WriteOptions{Confidence: &label})
	if err != nil || shared.AgentID != memory.SharedAgentID || shared.Confidence != label || fake.launches != 1 {
		t.Fatalf("explicit shared scheduling: %+v err=%v launches=%d", shared, err, fake.launches)
	}
	_, err = backend.PutWithOptionsReturningFact(context.Background(), memory.SharedAgentID, "generic shared database policy", memory.WriteOptions{Confidence: &label})
	if err != nil || fake.launches != 2 {
		t.Fatalf("generic shared scheduling: err=%v launches=%d", err, fake.launches)
	}
	invalid := ""
	if _, err := backend.PutWithOptionsReturningFact(context.Background(), "a", "must not land", memory.WriteOptions{Confidence: &invalid}); err == nil {
		t.Fatal("invalid label accepted")
	}
	if fake.launches != 2 {
		t.Fatal("failed write triggered consolidation")
	}
	// Optional methods on the adapter cannot advertise support the actual
	// wrapped backend lacks. Older AdvancedStore fakes stay usable.
	capabilities := backend.ConfidenceCapabilities()
	if len(capabilities) != 2 || capabilities[0] != rpc.ConfidenceWriteV1 || capabilities[1] != rpc.ConfidenceSharedWriteV1 {
		t.Fatalf("unsupported capabilities advertised: %v", capabilities)
	}
	if _, err := backend.RecallWithOptions(context.Background(), "a", "database", 8, memory.RecallOptions{MinConfidence: &label}); !errors.Is(err, memory.ErrConfidenceUnsupported) {
		t.Fatalf("legacy optional recall error=%v", err)
	}
}

func TestConfidenceBackendConfiguredDefaultDirectRPCParity(t *testing.T) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.AsyncConsolidate = false
	cfg.ConfidenceWeight = 0.3
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close() })
	store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir(), StrictWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.Put(ctx, "a", "database policy"); err != nil {
		t.Fatal(err)
	}
	backend := rememberBackend{AdvancedStore: store, mem: mem}
	dir := t.TempDir()
	listener, cleanup, err := rpc.Listen(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	server := rpc.NewServer(backend, cfg)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(server.Stop)
	client, err := rpc.Dial(rpc.DialOptions{DataDir: dir, PingOnDial: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	actual, err := client.RecallWithOptions(ctx, "a", "database", 8, memory.RecallOptions{})
	if err != nil || actual.Retrieval == nil || actual.Retrieval.ConfidenceWeight != 0.3 || client.DefaultConfidenceWeight() != 0.3 {
		t.Fatalf("daemon configured default lost: %+v %v provider=%g", actual, err, client.DefaultConfidenceWeight())
	}
	expected, err := backend.RecallWithOptions(ctx, "a", "database", 8, memory.RecallOptions{})
	if err != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("daemon configured default mismatch: actual=%+v expected=%+v err=%v", actual, expected, err)
	}
	explain, err := backend.RecallExplainWithOptions(ctx, "a", "database", 8, memory.RecallOptions{})
	if err != nil || explain.Retrieval == nil || len(explain.Receipts) != 1 || explain.Receipts[0].Ranking == nil || explain.Receipts[0].Ranking.ConfidenceWeight != 0.3 {
		t.Fatalf("daemon explain configured default=%+v %v", explain, err)
	}
	all, err := backend.RecallAllWithOptions(ctx, "a", "database", 8, memory.RecallOptions{})
	if err != nil || all.Retrieval == nil || all.Retrieval.ConfidenceWeight != 0.3 {
		t.Fatalf("daemon all configured default=%+v %v", all, err)
	}
}

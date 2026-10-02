package main

import (
	"context"
	"errors"
	"io"
	netrpc "net/rpc"
	"reflect"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

type confidenceWrapperFake struct {
	fakeStore
	mutations     int
	reads         int
	err           error
	writeOptions  memory.WriteOptions
	recallOptions memory.RecallOptions
	victims       []memory.Fact
}

func (s *confidenceWrapperFake) PutSharedWithOptionsReturningFact(ctx context.Context, text string, opts memory.WriteOptions) (memory.Fact, error) {
	return s.PutWithOptionsReturningFact(ctx, memory.SharedAgentID, text, opts)
}

func (s *confidenceWrapperFake) PutWithOptionsReturningFact(ctx context.Context, a, text string, opts memory.WriteOptions) (memory.Fact, error) {
	s.mutations++
	s.writeOptions = opts
	return memory.Fact{ID: "committed", AgentID: a, Text: text}, s.err
}
func (s *confidenceWrapperFake) ReviseWithOptions(ctx context.Context, a, target, text string, opts memory.WriteOptions) (memory.Fact, error) {
	return s.PutWithOptionsReturningFact(ctx, a, text, opts)
}
func (s *confidenceWrapperFake) ReviseFactsWithOptions(ctx context.Context, a, text string, opts memory.WriteOptions, victims ...memory.Fact) (memory.Fact, error) {
	s.victims = victims
	return s.PutWithOptionsReturningFact(ctx, a, text, opts)
}
func (s *confidenceWrapperFake) RecallWithOptions(ctx context.Context, a, q string, k int, opts memory.RecallOptions) (memory.RecallResult, error) {
	s.reads++
	s.recallOptions = opts
	return memory.RecallResult{Facts: []string{"result"}}, s.err
}
func (s *confidenceWrapperFake) RecallAllWithOptions(ctx context.Context, a, q string, k int, opts memory.RecallOptions) (memory.RecallResult, error) {
	return s.RecallWithOptions(ctx, a, q, k, opts)
}
func (s *confidenceWrapperFake) RecallExplainWithOptions(ctx context.Context, a, q string, k int, opts memory.RecallOptions) (memory.RecallExplainResult, error) {
	_, err := s.RecallWithOptions(ctx, a, q, k, opts)
	return memory.RecallExplainResult{Receipts: []memory.RecallReceipt{{Text: "result"}}}, err
}
func (s *confidenceWrapperFake) Retire(a string, victims ...memory.Fact) error {
	s.mutations++
	s.victims = victims
	return s.err
}
func (s *confidenceWrapperFake) SetPinned(a string, pinned bool, victims ...memory.Fact) error {
	return s.Retire(a, victims...)
}

func TestConfidenceWrapperNeverReplaysUncertainMutations(t *testing.T) {
	label := "verified"
	opts := memory.WriteOptions{Confidence: &label}
	victim := memory.Fact{ID: "victim", AgentID: "a", Text: "old"}
	for _, failure := range []error{io.EOF, io.ErrUnexpectedEOF, netrpc.ErrShutdown, errors.New("rpc: timed out; connection closed")} {
		for _, operation := range []string{"put", "put-shared", "revise", "revise-facts", "retire", "pin"} {
			t.Run(operation+"/"+failure.Error(), func(t *testing.T) {
				original := &confidenceWrapperFake{err: failure}
				fresh := &confidenceWrapperFake{}
				reopens := 0
				r := newReconnectingStoreAt(original, func() (cliStore, error) { reopens++; return fresh, nil })
				var err error
				ctx := context.Background()
				switch operation {
				case "put":
					_, err = r.PutWithOptionsReturningFact(ctx, "a", "new", opts)
				case "put-shared":
					_, err = r.PutSharedWithOptionsReturningFact(ctx, "new", opts)
				case "revise":
					_, err = r.ReviseWithOptions(ctx, "a", "old", "new", opts)
				case "revise-facts":
					_, err = r.ReviseFactsWithOptions(ctx, "a", "new", opts, victim)
				case "retire":
					err = r.Retire("a", victim)
				case "pin":
					err = r.SetPinned("a", true, victim)
				}
				if !errors.Is(err, failure) || original.mutations != 1 || fresh.mutations != 0 || reopens != 0 {
					t.Fatalf("uncertain write replayed: err=%v original=%d fresh=%d reopens=%d", err, original.mutations, fresh.mutations, reopens)
				}
			})
		}
	}
}

func TestDirectConfidenceResolvesConfiguredDefaultBeforeStore(t *testing.T) {
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
	// Use a backend with another default to prove the product boundary, not
	// the storage object's coincidentally matching configuration, resolves it.
	store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir(), StrictWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	d := &directStore{mem: mem, store: store}
	ctx := context.Background()
	if err := store.Put(ctx, "a", "database policy"); err != nil {
		t.Fatal(err)
	}
	result, err := d.RecallWithOptions(ctx, "a", "database", 8, memory.RecallOptions{})
	if err != nil || result.Retrieval == nil || result.Retrieval.ConfidenceWeight != 0.3 || d.DefaultConfidenceWeight() != 0.3 {
		t.Fatalf("direct configured default=%+v %v provider=%g", result, err, d.DefaultConfidenceWeight())
	}
	explain, err := d.RecallExplainWithOptions(ctx, "a", "database", 8, memory.RecallOptions{})
	if err != nil || explain.Retrieval == nil || len(explain.Receipts) != 1 || explain.Receipts[0].Ranking == nil || explain.Receipts[0].Ranking.ConfidenceWeight != 0.3 {
		t.Fatalf("direct explain configured default=%+v %v", explain, err)
	}
	all, err := d.RecallAllWithOptions(ctx, "a", "database", 8, memory.RecallOptions{})
	if err != nil || all.Retrieval == nil || all.Retrieval.ConfidenceWeight != 0.3 {
		t.Fatalf("direct all configured default=%+v %v", all, err)
	}
}

func TestConfidenceWrapperOptionsAndReconnect(t *testing.T) {
	label := "verified"
	weight := 0.2
	writeOpts := memory.WriteOptions{Confidence: &label}
	readOpts := memory.RecallOptions{MinConfidence: &label, ConfidenceWeight: &weight}
	original := &confidenceWrapperFake{err: io.EOF}
	fresh := &confidenceWrapperFake{}
	reopens := 0
	r := newReconnectingStoreAt(original, func() (cliStore, error) { reopens++; return fresh, nil })
	ctx := context.Background()
	result, err := r.RecallWithOptions(ctx, "a", "q", 3, readOpts)
	if err != nil || len(result.Facts) != 1 || reopens != 1 || !reflect.DeepEqual(fresh.recallOptions, readOpts) {
		t.Fatalf("recall options lost: %+v err=%v reopens=%d opts=%+v", result, err, reopens, fresh.recallOptions)
	}
	if _, err := r.RecallAllWithOptions(ctx, "a", "q", 3, readOpts); err != nil {
		t.Fatal(err)
	}
	if _, err := r.RecallExplainWithOptions(ctx, "a", "q", 3, readOpts); err != nil {
		t.Fatal(err)
	}
	victim := memory.Fact{ID: "exact", AgentID: "a", Text: "old"}
	if _, err := r.ReviseFactsWithOptions(ctx, "a", "new", writeOpts, victim); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.writeOptions, writeOpts) || !reflect.DeepEqual(fresh.victims, []memory.Fact{victim}) {
		t.Fatalf("writer options/IDs lost: %+v %+v", fresh.writeOptions, fresh.victims)
	}
	if fresh.mutations != 1 || fresh.reads != 3 {
		t.Fatalf("unexpected calls: writes=%d reads=%d", fresh.mutations, fresh.reads)
	}
}

func TestConfidenceWrapperUnsupportedNeverReconnects(t *testing.T) {
	legacy := &fakeStore{}
	reopens := 0
	r := newReconnectingStoreAt(legacy, func() (cliStore, error) { reopens++; return legacy, nil })
	label := "verified"
	_, err := r.PutWithOptionsReturningFact(context.Background(), "a", "new", memory.WriteOptions{Confidence: &label})
	if !errors.Is(err, memory.ErrConfidenceUnsupported) || reopens != 0 {
		t.Fatalf("unsupported write err=%v reconnects=%d", err, reopens)
	}
	_, err = r.RecallWithOptions(context.Background(), "a", "q", 8, memory.RecallOptions{MinConfidence: &label})
	if !errors.Is(err, memory.ErrConfidenceUnsupported) || reopens != 0 {
		t.Fatalf("unsupported read err=%v reconnects=%d", err, reopens)
	}
	invalid := ""
	_, err = r.PutWithOptionsReturningFact(context.Background(), "a", "new", memory.WriteOptions{Confidence: &invalid})
	if err == nil || reopens != 0 {
		t.Fatalf("invalid opts err=%v reconnects=%d", err, reopens)
	}
}

type confidenceAsyncStore struct {
	graymatter.AdvancedStore
	rpc.ConfidenceWriter
	launches int
	observed []memory.Fact
}

func (s *confidenceAsyncStore) LaunchAsyncConsolidate(a string, cfg memory.ConsolidateConfig) {
	s.launches++
	s.observed, _ = s.AdvancedStore.List(a)
}

func (s *confidenceAsyncStore) PutSharedWithOptionsReturningFact(ctx context.Context, text string, opts memory.WriteOptions) (memory.Fact, error) {
	return s.AdvancedStore.(rpc.SharedConfidenceWriter).PutSharedWithOptionsReturningFact(ctx, text, opts)
}

func TestDirectConfidenceWriteLaunchesAfterDurableMetadata(t *testing.T) {
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
	fake := &confidenceAsyncStore{AdvancedStore: advanced, ConfidenceWriter: advanced.(rpc.ConfidenceWriter)}
	d := &directStore{mem: mem, store: fake}
	label := "verified"
	fact, err := d.PutWithOptionsReturningFact(context.Background(), "a", "database policy", memory.WriteOptions{Confidence: &label})
	if err != nil || fake.launches != 1 || len(fake.observed) != 1 || fake.observed[0].ID != fact.ID || fake.observed[0].Confidence != label {
		t.Fatalf("consolidation before metadata or incorrect dispatch: fact=%+v err=%v launches=%d observed=%+v", fact, err, fake.launches, fake.observed)
	}
	shared, err := d.PutSharedWithOptionsReturningFact(context.Background(), "shared database policy", memory.WriteOptions{Confidence: &label})
	if err != nil || shared.AgentID != memory.SharedAgentID || shared.Confidence != label || fake.launches != 1 {
		t.Fatalf("explicit shared write changed async policy: %+v err=%v launches=%d", shared, err, fake.launches)
	}
	_, err = d.PutWithOptionsReturningFact(context.Background(), memory.SharedAgentID, "generic shared database policy", memory.WriteOptions{Confidence: &label})
	if err != nil || fake.launches != 2 {
		t.Fatalf("generic shared namespace write lost async policy: err=%v launches=%d", err, fake.launches)
	}
	invalid := "invalid"
	if _, err := d.PutWithOptionsReturningFact(context.Background(), "a", "must not land", memory.WriteOptions{Confidence: &invalid}); err == nil {
		t.Fatal("invalid confidence accepted")
	}
	if fake.launches != 2 {
		t.Fatal("invalid write launched consolidation")
	}
}

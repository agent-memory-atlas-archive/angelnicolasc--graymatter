package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func TestRecallPreviewCancellationBeforeReadAndAfterEmbedding(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprint(indexed), func(t *testing.T) {
			s := inspectionStore(t, indexed)
			inspectionPut(t, s, "a", "database policy")
			calls := 0
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			s.embedder = inspectionCallbackEmbedder{callback: func() { calls++ }}
			txID := inspectionTxID(t, s)
			if _, err := s.RecallPreview(ctx, "a", "database", 8); !errors.Is(err, context.Canceled) {
				t.Fatalf("pre-cancelled preview: %v", err)
			}
			if calls != 0 {
				t.Fatal("cancelled request called embedder")
			}
			ctx, cancel = context.WithCancel(context.Background())
			defer cancel()
			s.embedder = inspectionCallbackEmbedder{callback: func() { calls++; cancel() }}
			if receipts, err := s.RecallPreview(ctx, "a", "database", 8); !errors.Is(err, context.Canceled) || len(receipts) != 0 {
				t.Fatalf("cancel after embedding became success: %+v %v", receipts, err)
			}
			if calls != 1 || inspectionTxID(t, s) != txID {
				t.Fatal("cancelled preview caused side effects")
			}
		})
	}
}

// Cancels on a deterministic read boundary, avoiding scheduling/timing flakes
// while proving a large scan/ranking stage checks cancellation incrementally.
type cancelAfterChecks struct {
	context.Context
	cancel        context.CancelFunc
	checks, limit int
}

func (c *cancelAfterChecks) Err() error {
	c.checks++
	if c.checks == c.limit {
		c.cancel()
	}
	return c.Context.Err()
}
func cancellationBudget(limit int) *cancelAfterChecks {
	ctx, cancel := context.WithCancel(context.Background())
	return &cancelAfterChecks{Context: ctx, cancel: cancel, limit: limit}
}

func TestRecallPreviewLargeCorpusCancellationIsIncremental(t *testing.T) {
	s := inspectionStore(t, false)
	const size = 20000
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b, err := tx.Bucket(bucketFacts).CreateBucketIfNotExists([]byte("a"))
		if err != nil {
			return err
		}
		indexed, err := tx.Bucket(bucketFacts).CreateBucketIfNotExists([]byte("indexed"))
		if err != nil {
			return err
		}
		for i := 0; i < size; i++ {
			f := Fact{ID: fmt.Sprintf("%026d", i), AgentID: "a", Text: "database service policy and production cache", Weight: 1, CreatedAt: time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)}
			raw, err := json.Marshal(f)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(f.ID), raw); err != nil {
				return err
			}
			// The indexed case needs only more rows than the cancellation budget;
			// avoid making every race run rebuild an unrelated 20k-row index.
			if i < 512 {
				f.AgentID = "indexed"
				raw, err = json.Marshal(f)
				if err != nil {
					return err
				}
				if err := indexed.Put([]byte(f.ID), raw); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	txID := inspectionTxID(t, s)
	ctx := cancellationBudget(64)
	defer ctx.cancel()
	if _, err := s.RecallPreview(ctx, "a", "database", 8); !errors.Is(err, context.Canceled) {
		t.Fatalf("scan cancellation: %v", err)
	}
	if ctx.checks > ctx.limit+2 {
		t.Fatalf("scan continued after cancellation: %d checks", ctx.checks)
	}
	// Explicitly test the CPU scoring phase as well as database decoding.
	facts, err := s.listLite("a")
	if err != nil {
		t.Fatal(err)
	}
	ctx = cancellationBudget(64)
	defer ctx.cancel()
	if _, _, _, err := keywordScoreDetailedContext(ctx, "database", facts, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("scoring cancellation: %v", err)
	}
	if ctx.checks > ctx.limit+2 {
		t.Fatalf("scoring continued after cancellation: %d checks", ctx.checks)
	}
	if inspectionTxID(t, s) != txID {
		t.Fatal("cancelled scan/scoring wrote database")
	}
	// A preview on a populated candidate index must also stop before loading
	// the entire spine. Building this fixture index is outside the inspection.
	if err := s.idxRebuild("indexed"); err != nil {
		t.Fatal(err)
	}
	s.cfg.CandidateRetrieval = true
	txID = inspectionTxID(t, s)
	ctx = cancellationBudget(64)
	defer ctx.cancel()
	if _, err := s.RecallPreview(ctx, "indexed", "database", 8); !errors.Is(err, context.Canceled) {
		t.Fatalf("indexed cancellation: %v", err)
	}
	if ctx.checks > ctx.limit+3 {
		t.Fatalf("index continued after cancellation: %d checks", ctx.checks)
	}
	if inspectionTxID(t, s) != txID {
		t.Fatal("cancelled inspection wrote database")
	}
}

package memory

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"
)

func openLifecycleNoopStore(t *testing.T, indexed bool) *Store {
	t.Helper()
	s, err := Open(StoreConfig{DataDir: t.TempDir(), CandidateRetrieval: indexed})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Pinned facts are exempt from decay. Even committing an empty bbolt write
// transaction writes metadata, so checking unchanged fact text is insufficient.
func TestPinnedConsolidationDoesNotWrite(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		t.Run(fmt.Sprintf("indexed=%t", indexed), func(t *testing.T) {
			s := openLifecycleNoopStore(t, indexed)
			ctx := context.Background()
			for i := 0; i < 4; i++ {
				f, err := s.PutReturningFact(ctx, "a", fmt.Sprintf("permanent policy %d", i))
				if err != nil {
					t.Fatal(err)
				}
				if err := s.SetPinned("a", true, f); err != nil {
					t.Fatal(err)
				}
			}
			beforeFacts, err := s.List("a")
			if err != nil {
				t.Fatal(err)
			}
			before := s.db.Stats()
			if err := s.Consolidate(ctx, "a", &testConsolidateCfg{threshold: 20, halfLife: 24 * time.Hour}); err != nil {
				t.Fatal(err)
			}
			after := s.db.Stats()
			if writes := after.TxStats.Write - before.TxStats.Write; writes != 0 {
				t.Errorf("pinned consolidation performed %d bbolt writes, want none", writes)
			}
			afterFacts, err := s.List("a")
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(afterFacts, beforeFacts) {
				t.Fatalf("pinned facts changed: before=%+v after=%+v", beforeFacts, afterFacts)
			}
		})
	}
}

// Decay must decide whether to write using the pin and metadata it reads after
// acquiring the writer lock, and return that current fact even on the no-op path.
func TestDecayUsesConcurrentPinState(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		for _, pinned := range []bool{false, true} {
			t.Run(fmt.Sprintf("indexed=%t/pinned=%t", indexed, pinned), func(t *testing.T) {
				s := openLifecycleNoopStore(t, indexed)
				old, err := s.PutReturningFact(context.Background(), "a", "permanent policy")
				if err != nil {
					t.Fatal(err)
				}
				if err := s.SetPinned("a", !pinned, old); err != nil {
					t.Fatal(err)
				}
				tx, err := s.db.Begin(true)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				type outcome struct {
					fact Fact
					err  error
				}
				started := make(chan struct{})
				done := make(chan outcome, 1)
				go func() {
					close(started)
					fact, err := s.decayFact("a", old.ID, old.AccessedAt.Add(48*time.Hour), math.Log(2)/24)
					done <- outcome{fact: fact, err: err}
				}()
				<-started
				current, err := factInTx(tx, "a", old.ID)
				if err != nil {
					t.Fatal(err)
				}
				current.Pinned = pinned
				current.PinnedAt = time.Time{}
				if pinned {
					current.PinnedAt = old.CreatedAt.Add(time.Hour)
				}
				current.Confidence = "unverified"
				current.AccessCount = 17
				current.AccessedAt = old.AccessedAt.Add(24 * time.Hour)
				current.Weight = .7
				if err := s.persistFactTx(tx, tx.Bucket(bucketFacts).Bucket([]byte("a")), "a", current); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				if !pinned {
					current.Weight = .5
				}
				result := <-done
				if result.err != nil {
					t.Fatal(result.err)
				}
				if !reflect.DeepEqual(result.fact, current) {
					t.Fatalf("decay returned stale state: got=%+v want=%+v", result.fact, current)
				}
				stored, ok := s.loadFact("a", old.ID)
				if !ok || !reflect.DeepEqual(stored, current) {
					t.Fatalf("decay lost concurrent metadata: got=%+v want=%+v", stored, current)
				}
			})
		}
	}
}

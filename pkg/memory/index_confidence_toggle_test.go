package memory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func openConfidenceToggleStore(t *testing.T, dir string, indexed, readOnly bool) *Store {
	t.Helper()
	s, err := Open(StoreConfig{DataDir: dir, CandidateRetrieval: indexed, ReadOnly: readOnly})
	if err != nil {
		t.Fatal(err)
	}
	s.now = func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// Reopening with candidate retrieval disabled is supported. A same-count
// change must invalidate the old index even though its version, tokenisation
// and fact count still match. Exercise both a confidence-only write and a
// delete/add pair, then compare retrieval against the canonical snapshot.
func TestIndexConfidenceReopenAfterScanWrites(t *testing.T) {
	ctx := context.Background()
	const agent = "toggle"
	for _, mutation := range []struct {
		name, before, after string
		replace             bool
	}{
		{"downgrade", "verified", "unverified", false},
		{"promotion", "unverified", "verified", false},
		{"replace_downgrade", "verified", "unverified", true},
		{"replace_promotion", "unverified", "verified", true},
	} {
		for _, readOnly := range []bool{false, true} {
			for _, writeBeforeRecall := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/read_only=%t/write_before_recall=%t", mutation.name, readOnly, writeBeforeRecall), func(t *testing.T) {
					dir := t.TempDir()
					s := openConfidenceToggleStore(t, dir, true, false)
					f, err := s.PutWithOptionsReturningFact(ctx, agent, "routing deployment", WriteOptions{Confidence: &mutation.before})
					if err != nil {
						t.Fatal(err)
					}
					for _, text := range []string{"routing schedule", "orchard pruning"} {
						if err := s.Put(ctx, agent, text); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := s.Recall(ctx, agent, "routing", 8); err != nil {
						t.Fatal(err)
					}
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}

					s = openConfidenceToggleStore(t, dir, false, false)
					if mutation.replace {
						if err := s.Delete(agent, f.ID); err != nil {
							t.Fatal(err)
						}
						if _, err := s.PutWithOptionsReturningFact(ctx, agent, "routing replacement", WriteOptions{Confidence: &mutation.after}); err != nil {
							t.Fatal(err)
						}
					} else {
						f.Confidence = mutation.after
						if err := s.UpdateFact(agent, f); err != nil {
							t.Fatal(err)
						}
					}
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}

					if writeBeforeRecall {
						s = openConfidenceToggleStore(t, dir, true, false)
						// This write must not declare a partially maintained index
						// healthy before the first recall gets a chance to rebuild.
						if err := s.Put(ctx, agent, "orchard harvest"); err != nil {
							t.Fatal(err)
						}
						if err := s.Close(); err != nil {
							t.Fatal(err)
						}
					}

					s = openConfidenceToggleStore(t, dir, false, true)
					snapshot, err := s.List(agent)
					if err != nil {
						t.Fatal(err)
					}
					weight, minimum := .2, "verified"
					options := []RecallOptions{{ConfidenceWeight: &weight}, {ConfidenceWeight: &weight, MinConfidence: &minimum}}
					scans := make([]RecallExplainResult, len(options))
					for i, opts := range options {
						scans[i], err = s.RecallExplainWithOptions(ctx, agent, "routing", 50, opts)
						if err != nil {
							t.Fatal(err)
						}
						assertConfidenceToggleSnapshot(t, snapshot, opts, scans[i].Receipts)
					}
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}

					s = openConfidenceToggleStore(t, dir, true, readOnly)
					for i, opts := range options {
						got, err := s.RecallExplainWithOptions(ctx, agent, "routing", 50, opts)
						if err != nil {
							t.Fatal(err)
						}
						assertConfidenceToggleSnapshot(t, snapshot, opts, got.Receipts)
						if !reflect.DeepEqual(scans[i], got) {
							t.Errorf("indexed receipts differ from canonical scan: got %+v, want %+v", got, scans[i])
						}
						plain, err := s.RecallWithOptions(ctx, agent, "routing", 50, opts)
						if err != nil {
							t.Fatal(err)
						}
						if len(plain.Facts) != len(scans[i].Receipts) {
							t.Fatalf("plain count=%d, canonical count=%d", len(plain.Facts), len(scans[i].Receipts))
						}
						for j, receipt := range scans[i].Receipts {
							if plain.Facts[j] != receipt.Text {
								t.Errorf("plain fact %d=%q, canonical=%q", j, plain.Facts[j], receipt.Text)
							}
						}
					}
				})
			}
		}
	}
}

func assertConfidenceToggleSnapshot(t *testing.T, snapshot []Fact, opts RecallOptions, receipts []RecallReceipt) {
	t.Helper()
	want := independentConfidenceExpected(snapshot, "routing", opts.MinConfidence, *opts.ConfidenceWeight, SignalWeights{Keyword: 1, Recency: .5}, 0)
	if len(receipts) != len(want) {
		t.Errorf("receipt count=%d, canonical count=%d", len(receipts), len(want))
		return
	}
	for i, r := range receipts {
		expected := want[i]
		if r.Provenance.FactID != expected.fact.ID || r.Text != expected.fact.Text || r.Provenance.Confidence != expected.fact.Confidence {
			t.Errorf("receipt %d does not describe the selected canonical fact: got %+v, want %+v", i, r, expected.fact)
		}
		if r.Ranking == nil {
			t.Fatal("missing confidence ranking receipt")
		}
		if r.Ranks.KeywordRank != expected.keyword || r.Ranks.RecencyRank != expected.recency || math.Abs(r.Ranking.BaseScore-expected.base) > 1e-15 || math.Abs(r.Ranking.FinalScore-expected.final) > 1e-15 {
			t.Errorf("receipt %d has stale ranking: got %+v, canonical base=%g final=%g", i, r, expected.base, expected.final)
		}
		if math.Abs(r.Ranking.FinalScore-r.Ranking.BaseScore*r.Ranking.Factor) > 1e-15 {
			t.Errorf("receipt %d score contradicts its confidence factor: %+v", i, r.Ranking)
		}
	}
}

// An indexless store must retain its cheap write path. Invalidating a previous
// index must not create index buckets for a store that never had one.
func TestIndexConfidenceScanWritesDoNotCreateIndex(t *testing.T) {
	s := openConfidenceToggleStore(t, t.TempDir(), false, false)
	f, err := s.PutReturningFact(context.Background(), "scan", "routing deployment")
	if err != nil {
		t.Fatal(err)
	}
	f.Confidence = "verified"
	if err := s.UpdateFact("scan", f); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("scan", f.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.View(func(tx *bolt.Tx) error {
		for _, name := range [][]byte{bucketIdxMeta, bucketIdxTerms, bucketIdxRecency} {
			if tx.Bucket(name) != nil {
				t.Errorf("scan-only mutations created index bucket %q", name)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIndexConfidenceInvalidationPreservesCanonicalSnapshot(t *testing.T) {
	s := openConfidenceToggleStore(t, t.TempDir(), true, false)
	ctx := context.Background()
	const agent = "snapshot"
	verified, weight := "verified", .2
	f, err := s.PutWithOptionsReturningFact(ctx, agent, "routing deployment", WriteOptions{Confidence: &verified})
	if err != nil {
		t.Fatal(err)
	}
	// Warm both the process-local count verification and the spine cache.
	if _, err := s.RecallWithOptions(ctx, agent, "routing", 8, RecallOptions{ConfidenceWeight: &weight}); err != nil {
		t.Fatal(err)
	}
	state := func() indexState {
		t.Helper()
		var st indexState
		if err := s.db.View(func(tx *bolt.Tx) error {
			var found bool
			st, found = idxReadState(tx, agent)
			if !found {
				t.Fatal("fixture has no persisted index state")
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		return st
	}
	before := state()
	f, ok := s.loadFact(agent, f.ID)
	if !ok {
		t.Fatal("fixture fact missing")
	}
	changed := f
	changed.Confidence = "unverified"
	s.cfg.CandidateRetrieval = false
	abort := errors.New("abort canonical mutation")
	if err := s.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFacts).Bucket([]byte(agent))
		if err := s.persistFactTx(tx, b, agent, changed); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatalf("rollback: %v", err)
	}
	if current, ok := s.loadFact(agent, f.ID); !ok || !reflect.DeepEqual(current, f) || state() != before {
		t.Fatal("aborted mutation changed the canonical fact or its index stamp")
	}
	if err := s.UpdateFact(agent, changed); err != nil {
		t.Fatal(err)
	}
	s.cfg.CandidateRetrieval = true
	// An indexed lifecycle patch before recall cannot validate stale entries.
	if err := s.SetPinned(agent, true, changed); err != nil {
		t.Fatal(err)
	}
	if usable, err := s.idxUsable(agent); err != nil || usable {
		t.Fatalf("partial maintenance rehabilitated stale index: usable=%t err=%v", usable, err)
	}
	if state().Writes <= before.Writes {
		t.Fatal("invalidation lost the cached spine's mutation generation")
	}
	got, err := s.RecallExplainWithOptions(ctx, agent, "routing", 8, RecallOptions{ConfidenceWeight: &weight})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Receipts) != 1 || got.Receipts[0].Provenance.Confidence != "unverified" || !got.Receipts[0].Provenance.Pinned {
		t.Fatalf("rebuilt index lost canonical metadata: %+v", got)
	}
	snapshot, err := s.List(agent)
	if err != nil {
		t.Fatal(err)
	}
	assertConfidenceToggleSnapshot(t, snapshot, RecallOptions{ConfidenceWeight: &weight}, got.Receipts)
	if usable, err := s.idxUsable(agent); err != nil || !usable || state().Writes <= before.Writes {
		t.Fatalf("rebuild did not restore a fresh index: usable=%t err=%v", usable, err)
	}
}

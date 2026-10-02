package memory

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	bolt "go.etcd.io/bbolt"
)

// BenchmarkConfidenceRecall measures the entire indexed options read path,
// including normal access bookkeeping. Setup commits a reproducible labeled
// corpus in bounded transactions; no external providers or existing stores are
// used. Bounded setup avoids quadratic unsplit bbolt node insertion at 30k.
func BenchmarkConfidenceRecall(b *testing.B) {
	for _, size := range []int{600, 5000, 10000, 30000} {
		for _, arm := range []string{"legacy", "detailed", "weighted", "filtered"} {
			b.Run(fmt.Sprintf("%d/%s", size, arm), func(b *testing.B) {
				s := confidenceBenchmarkStore(b, size)
				defer func() { _ = s.Close() }()
				opts := RecallOptions{}
				weight := .2
				minimum := "verified"
				if arm == "weighted" || arm == "filtered" {
					opts.ConfidenceWeight = &weight
				}
				if arm == "filtered" {
					opts.MinConfidence = &minimum
				}
				recall := func() []string {
					if arm == "legacy" {
						texts, err := s.Recall(context.Background(), "bench-confidence", "routing cutoff", 8)
						if err != nil {
							b.Fatal(err)
						}
						return texts
					}
					if arm == "detailed" {
						texts, _, err := s.RecallDetailed(context.Background(), "bench-confidence", "routing cutoff", 8)
						if err != nil {
							b.Fatal(err)
						}
						return texts
					}
					result, err := s.RecallWithOptions(context.Background(), "bench-confidence", "routing cutoff", 8, opts)
					if err != nil {
						b.Fatal(err)
					}
					return result.Facts
				}
				warmup := recall()
				if len(warmup) == 0 {
					b.Fatal("empty results cannot count as a performance improvement")
				}
				if arm == "filtered" {
					facts, err := s.List("bench-confidence")
					if err != nil {
						b.Fatal(err)
					}
					labels := make(map[string]string, len(facts))
					for _, fact := range facts {
						labels[fact.Text] = fact.Confidence
					}
					for _, text := range warmup {
						if labels[text] != "verified" {
							b.Fatal("filtered warmup returned ineligible text")
						}
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if got := recall(); len(got) != len(warmup) {
						b.Fatalf("result length changed: got %d, want %d", len(got), len(warmup))
					}
				}
				b.ReportMetric(float64(len(warmup)), "results")
			})
		}
	}
}

// BenchmarkConfidencePut compares durable legacy and explicit-label writes at
// each pre-existing corpus size. Run with a fixed small benchtime (e.g. 10x)
// when reporting size-specific write latency, so growth during the run is small.
func BenchmarkConfidencePut(b *testing.B) {
	for _, size := range []int{600, 5000, 10000, 30000} {
		for _, arm := range []string{"legacy", "verified"} {
			b.Run(fmt.Sprintf("%d/%s", size, arm), func(b *testing.B) {
				s := confidenceBenchmarkStore(b, size)
				defer func() { _ = s.Close() }()
				opts := WriteOptions{}
				confidence := "verified"
				if arm == "verified" {
					opts.Confidence = &confidence
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					fact, err := s.PutWithOptionsReturningFact(context.Background(), "bench-confidence", "routing cutoff benchmark write", opts)
					if err != nil {
						b.Fatal(err)
					}
					if fact.ID == "" || fact.Text != "routing cutoff benchmark write" {
						b.Fatal("write did not return its committed identity")
					}
					if arm == "verified" && fact.Confidence != confidence {
						b.Fatal("write lost its explicit confidence")
					}
				}
			})
		}
	}
}

func confidenceBenchmarkStore(b *testing.B, size int) *Store {
	b.Helper()
	s, err := Open(StoreConfig{DataDir: b.TempDir(), CandidateRetrieval: true, Embedder: embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword})})
	if err != nil {
		b.Fatal(err)
	}
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for start := 0; start < size; start += 512 {
		end := min(start+512, size)
		if err := s.db.Update(func(tx *bolt.Tx) error {
			bucket, err := tx.Bucket(bucketFacts).CreateBucketIfNotExists([]byte("bench-confidence"))
			if err != nil {
				return err
			}
			if err := tx.Bucket(bucketAgents).Put([]byte("bench-confidence"), []byte("1")); err != nil {
				return err
			}
			categories := []string{"verified", "inferred", "unverified"}
			for i := start; i < end; i++ {
				text := fmt.Sprintf("archive service deployment shard %06d retention", i)
				if i%127 == 0 {
					text += " routing cutoff"
				}
				f := Fact{ID: fmt.Sprintf("%026d", i), AgentID: "bench-confidence", Text: text, CreatedAt: stamp.Add(time.Duration(i) * time.Second), AccessedAt: stamp, Confidence: categories[i%3], Weight: 1}
				raw, err := f.marshal()
				if err != nil {
					return err
				}
				if err := bucket.Put([]byte(f.ID), raw); err != nil {
					return err
				}
				if err := idxAddFact(tx, f, false); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			_ = s.Close()
			b.Fatal(err)
		}
	}
	// Stamp committed bbolt statistics. Stats inside the final dirty transaction
	// can omit new keys and would make the first recall rebuild the whole index.
	if err := s.db.Update(func(tx *bolt.Tx) error { return idxSetCount(tx, "bench-confidence", false) }); err != nil {
		_ = s.Close()
		b.Fatal(err)
	}
	return s
}

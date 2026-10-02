package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// BenchmarkListFactsPage measures a 100-row first page including its exact
// total. Each canonical row carries a 384-dimensional embedding that the
// inspection decoder skips. Fixture creation is outside the timed section;
// one transaction avoids measuring thousands of independent fsyncs.
func BenchmarkListFactsPage(b *testing.B) {
	for _, size := range []int{100, 10000, 100000} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			s, err := Open(StoreConfig{DataDir: b.TempDir()})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = s.Close() })
			at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
			vector := make([]float32, 384)
			if err := s.db.Update(func(tx *bolt.Tx) error {
				bucket, err := tx.Bucket(bucketFacts).CreateBucketIfNotExists([]byte("bench"))
				if err != nil {
					return err
				}
				for i := 0; i < size; i++ {
					f := Fact{ID: fmt.Sprintf("%026d", i), AgentID: "bench", Text: fmt.Sprintf("Production database policy %d: retain backups for thirty days.", i), CreatedAt: at, AccessedAt: at, Weight: 1, Embedding: vector}
					raw, err := json.Marshal(f)
					if err != nil {
						return err
					}
					if err := bucket.Put([]byte(f.ID), raw); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				page, err := s.ListFacts(context.Background(), "bench", "all", "", "", 100)
				if err != nil || len(page.Facts) != 100 || page.Total != size {
					b.Fatalf("page: rows=%d total=%d err=%v", len(page.Facts), page.Total, err)
				}
			}
		})
	}
}

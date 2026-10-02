package rpc

import (
	"context"
	"reflect"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

func TestConfidenceLegacyAllDirectRPCBookkeepingParity(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		name := "discarded-shared"
		if duplicate {
			name = "duplicate-shared"
		}
		t.Run(name, func(t *testing.T) {
			dir, server := startServer(t, "")
			client := dialT(t, dir)
			store := server.backend.(*memory.Store)
			ctx := context.Background()
			selected, err := store.PutReturningFact(ctx, "all-parity", "routing selected")
			if err != nil {
				t.Fatal(err)
			}
			sharedText := "routing discarded shared"
			if duplicate {
				sharedText = selected.Text
			}
			shared, err := store.PutReturningFact(ctx, memory.SharedAgentID, sharedText)
			if err != nil {
				t.Fatal(err)
			}
			assertAccess := func(wantSelected int) {
				t.Helper()
				for _, namespace := range []string{"all-parity", memory.SharedAgentID} {
					facts, err := client.List(namespace)
					if err != nil || len(facts) != 1 {
						t.Fatalf("canonical identities lost: %+v / %v", facts, err)
					}
					wantID, wantAccess := selected.ID, wantSelected
					if namespace == memory.SharedAgentID {
						wantID, wantAccess = shared.ID, 0
					}
					if facts[0].ID != wantID || facts[0].AccessCount != wantAccess {
						t.Fatalf("discarded/duplicate identity received access: %+v; want id=%s access=%d", facts, wantID, wantAccess)
					}
				}
			}
			direct, err := store.RecallAll(ctx, "all-parity", "routing", 1)
			if err != nil || !reflect.DeepEqual(direct, []string{selected.Text}) {
				t.Fatalf("direct legacy selection: %v / %v", direct, err)
			}
			assertAccess(1)
			remote, err := client.RecallAll(ctx, "all-parity", "routing", 1)
			if err != nil || !reflect.DeepEqual(remote, direct) {
				t.Fatalf("legacy direct/RPC selection changed: direct=%v rpc=%v / %v", direct, remote, err)
			}
			assertAccess(2)
			for _, topK := range []int{0, -1} {
				direct, err = store.RecallAll(ctx, "all-parity", "routing", topK)
				if err != nil || len(direct) != 0 {
					t.Fatalf("direct raw nonpositive topK changed: %v / %v", direct, err)
				}
				remote, err = client.RecallAll(ctx, "all-parity", "routing", topK)
				if err != nil || len(remote) != 0 {
					t.Fatalf("RPC raw nonpositive topK changed: %v / %v", remote, err)
				}
			}
			assertAccess(2)
		})
	}
}

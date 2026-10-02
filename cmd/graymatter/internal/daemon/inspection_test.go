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

func TestInspectionBackendDirectRPCParity(t *testing.T) {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.AsyncConsolidate = false
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	ctx := context.Background()
	store := mem.Advanced()
	if err := store.Put(ctx, "a", "production database policy"); err != nil {
		t.Fatal(err)
	}
	backend := rememberBackend{AdvancedStore: store, mem: mem}
	listener, cleanup, err := rpc.Listen(cfg.DataDir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	server := rpc.NewServer(backend, cfg)
	defer server.Stop()
	go func() { _ = server.Serve(listener) }()
	client, err := rpc.Dial(rpc.DialOptions{DataDir: cfg.DataDir, PingOnDial: true})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	before, _ := store.List("a")
	direct, err := mem.RecallPreview(ctx, "a", "database")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := client.RecallPreview(ctx, "a", "database", 0)
	if err != nil || !reflect.DeepEqual(direct, remote) {
		t.Fatalf("preview parity: %+v %+v %v", direct, remote, err)
	}
	directPage, err := mem.ListFacts(ctx, "a", "all", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	remotePage, err := client.ListFacts(ctx, "a", "all", "", "", 10)
	if err != nil || !reflect.DeepEqual(directPage, remotePage) {
		t.Fatalf("page parity: %+v %+v %v", directPage, remotePage, err)
	}
	after, _ := store.List("a")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("daemon inspection touched facts")
	}
	f := remotePage.Facts[0]
	pinned, err := client.CurateFact(ctx, "a", f.ID, "pin", "", f)
	if err != nil || !pinned.Pinned {
		t.Fatalf("curate: %+v %v", pinned, err)
	}
	if _, err := client.CurateFact(ctx, "a", f.ID, "retire", "", f); !errors.Is(err, memory.ErrFactChanged) {
		t.Fatalf("stale snapshot: %v", err)
	}
	legacy := rememberBackend{AdvancedStore: struct{ graymatter.AdvancedStore }{store}, mem: mem}
	if caps := legacy.InspectionCapabilities(); len(caps) != 0 {
		t.Fatalf("legacy capabilities: %v", caps)
	}
	if _, err := legacy.RecallPreview(ctx, "a", "database", 8); !errors.Is(err, memory.ErrInspectionUnsupported) {
		t.Fatal(err)
	}
}

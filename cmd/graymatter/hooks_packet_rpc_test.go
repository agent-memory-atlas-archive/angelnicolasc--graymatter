package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

type blockedPacketBackend struct {
	*memory.Store
	release chan struct{}
	calls   atomic.Int32
}

func (b *blockedPacketBackend) Recall(context.Context, string, string, int) ([]string, error) {
	b.calls.Add(1)
	<-b.release
	return []string{"late project fact"}, nil
}

func (b *blockedPacketBackend) RecallShared(context.Context, string, int) ([]string, error) {
	b.calls.Add(1)
	return []string{"unexpected shared call"}, nil
}

func TestHookPacketRPC_DeadlineClosesConnectionWithoutRetry(t *testing.T) {
	dir := t.TempDir()
	store, err := memory.Open(memory.StoreConfig{DataDir: dir, StrictWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	backend := &blockedPacketBackend{Store: store, release: make(chan struct{})}
	server := rpc.NewServer(backend, nil)
	listener, cleanup, err := rpc.Listen(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	defer close(backend.release)
	client, err := rpc.Dial(rpc.DialOptions{DataDir: dir, PingOnDial: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	remote := newReconnectingStore(daemonStore{Client: &daemon.Client{Client: client}})
	reconnects := 0
	original := reopenStore
	reopenStore = func() (cliStore, error) { reconnects++; return nil, fmt.Errorf("unexpected hook redial") }
	defer func() { reopenStore = original }()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	block, receipt, err := hookRecallLexicalBlock(ctx, remote, "example", "project fact")
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("hook used default RPC timeout instead of its deadline: %v", elapsed)
	}
	if err == nil || block != "" || receipt.Project.Effective != "none" || receipt.Shared.Effective != "none" {
		t.Fatalf("expired RPC result: block=%q receipt=%+v error=%v", block, receipt, err)
	}
	if backend.calls.Load() != 1 || reconnects != 0 {
		t.Fatalf("work continued after deadline: calls=%d reconnects=%d", backend.calls.Load(), reconnects)
	}
	if err := client.Ping(); err == nil {
		t.Fatal("timed-out connection remained usable")
	}
}

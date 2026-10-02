package rpc

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	netrpc "net/rpc"
	"net/rpc/jsonrpc"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

type blockingInspectionEmbedder struct {
	block    atomic.Bool
	started  chan struct{}
	canceled chan struct{}
}

func (e *blockingInspectionEmbedder) Embed(ctx context.Context, _ string) ([]float32, error) {
	if !e.block.Load() {
		return []float32{1, 0}, nil
	}
	select {
	case e.started <- struct{}{}:
	default:
	}
	<-ctx.Done()
	select {
	case e.canceled <- struct{}{}:
	default:
	}
	return nil, ctx.Err()
}
func (*blockingInspectionEmbedder) Dimensions() int { return 2 }
func (*blockingInspectionEmbedder) Name() string    { return "synthetic-cancel-fixture" }

func inspectionEmbedderServer(t *testing.T) (*memory.Store, *Server, *Client, *blockingInspectionEmbedder) {
	t.Helper()
	e := &blockingInspectionEmbedder{started: make(chan struct{}, 1), canceled: make(chan struct{}, 1)}
	s, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir(), Embedder: e, StrictWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	server := NewServer(s, nil)
	dir := t.TempDir()
	ln, cleanup, err := Listen(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(server.Stop)
	return s, server, dialT(t, dir), e
}

func awaitInspectionSignal(t *testing.T, signal <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatalf("timeout waiting for %s", label)
	}
}
func awaitInspectionCleanup(t *testing.T, server *Server) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		server.inspectionMu.Lock()
		n := len(server.inspectionCalls)
		server.inspectionMu.Unlock()
		if n == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("inspection registry did not clean up")
}

func TestInspectionCancellationStopsEmbeddingAndPreservesSharedConnection(t *testing.T) {
	for _, mutation := range []bool{false, true} {
		t.Run(fmt.Sprintf("mutation=%v", mutation), func(t *testing.T) {
			s, server, client, embedder := inspectionEmbedderServer(t)
			fact, err := s.PutReturningFact(context.Background(), "a", "database policy")
			if err != nil {
				t.Fatal(err)
			}
			before, err := s.List("a")
			if err != nil {
				t.Fatal(err)
			}
			embedder.block.Store(true)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if mutation {
					_, err := client.CurateFact(ctx, "a", fact.ID, "revise", "new policy", fact)
					done <- err
				} else {
					_, err := client.RecallPreview(ctx, "a", "database", 8)
					done <- err
				}
			}()
			awaitInspectionSignal(t, embedder.started, "embedding")
			// A second request on the same connection remains usable while the
			// first blocks, and canceling one must not close the connection.
			if _, err := client.ListFacts(context.Background(), "a", "all", "", "", 10); err != nil {
				t.Fatal(err)
			}
			cancel()
			select {
			case err := <-done:
				want := context.Canceled
				if mutation {
					want = memory.ErrMutationOutcomeUnknown
				}
				if !errors.Is(err, want) {
					t.Fatalf("got %v, want %v", err, want)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled client waited for call timeout")
			}
			awaitInspectionSignal(t, embedder.canceled, "remote cancellation")
			awaitInspectionCleanup(t, server)
			after, err := s.List("a")
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("cancellation changed facts: before=%+v after=%+v err=%v", before, after, err)
			}
			if err := client.Ping(); err != nil {
				t.Fatalf("cancellation closed shared connection: %v", err)
			}
		})
	}
}

func TestInspectionServerHonorsWireDeadline(t *testing.T) {
	s, server, client, embedder := inspectionEmbedderServer(t)
	if _, err := s.PutReturningFact(context.Background(), "a", "database policy"); err != nil {
		t.Fatal(err)
	}
	embedder.block.Store(true)
	meta := InspectionContext{RequestID: "0123456789abcdef0123456789abcdef", DeadlineUnixNano: time.Now().Add(60 * time.Millisecond).UnixNano()}
	var response RecallExplainResponse
	err := client.rpc.Call(ServiceName+".RecallPreview", &RecallPreviewRequest{Context: meta, AgentID: "a", Query: "database", TopK: 8}, &response)
	if !errors.Is(inspectionError(err), context.DeadlineExceeded) {
		t.Fatalf("deadline not enforced: %v", err)
	}
	awaitInspectionSignal(t, embedder.canceled, "deadline cancellation")
	awaitInspectionCleanup(t, server)
	if err := client.Ping(); err != nil {
		t.Fatal(err)
	}
}

// DelayedInspectionNegotiation models an old/busy daemon delaying the initial
// Ping. Cancellation here must never dispatch a subsequent mutation.
type DelayedInspectionNegotiation struct {
	started   chan struct{}
	release   chan struct{}
	mutations atomic.Int64
}

func (s *DelayedInspectionNegotiation) Ping(_ *PingRequest, resp *PingResponse) error {
	select {
	case s.started <- struct{}{}:
	default:
	}
	<-s.release
	resp.Protocol = Protocol
	resp.Capabilities = []string{InspectionV1, CurationV1, InspectionHealthV1, InspectionContextV1}
	return nil
}
func (s *DelayedInspectionNegotiation) CurateFact(_ *CurateFactRequest, _ *CurateFactResponse) error {
	s.mutations.Add(1)
	return nil
}

func pipeInspectionClient(t *testing.T, service any) *Client {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	server := netrpc.NewServer()
	if err := server.RegisterName(ServiceName, service); err != nil {
		t.Fatal(err)
	}
	go server.ServeCodec(jsonrpc.NewServerCodec(serverConn))
	client := &Client{rpc: netrpc.NewClientWithCodec(jsonrpc.NewClientCodec(clientConn)), conn: clientConn, callTimeout: 3 * time.Second}
	t.Cleanup(func() { _ = client.Close(); _ = serverConn.Close() })
	return client
}

func TestInspectionCancellationDuringNegotiationNeverDispatchesMutation(t *testing.T) {
	svc := &DelayedInspectionNegotiation{started: make(chan struct{}, 1), release: make(chan struct{})}
	client := pipeInspectionClient(t, svc)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.CurateFact(ctx, "a", "id", "pin", "", memory.Fact{}); done <- err }()
	awaitInspectionSignal(t, svc.started, "capability negotiation")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || errors.Is(err, memory.ErrMutationOutcomeUnknown) {
			t.Fatalf("pre-dispatch result: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("capability negotiation ignored context")
	}
	close(svc.release)
	if err := client.Ping(); err != nil {
		t.Fatal(err)
	}
	if svc.mutations.Load() != 0 {
		t.Fatal("canceled negotiation dispatched mutation")
	}
}

func TestInspectionRequiresContextCapabilityAndRejectsMissingMetadata(t *testing.T) {
	dir, server := startServer(t, "")
	client := dialT(t, dir)
	client.capMu.Lock()
	delete(client.capabilities, InspectionContextV1)
	client.capMu.Unlock()
	if _, err := client.CurateFact(context.Background(), "a", "id", "pin", "", memory.Fact{}); !errors.Is(err, memory.ErrInspectionUnsupported) {
		t.Fatalf("old context capability accepted: %v", err)
	}
	if err := server.ListFacts(&FactPageRequest{AgentID: "a"}, &FactPageResponse{}); err == nil {
		t.Fatal("missing context accepted")
	}
}

type CommitThenWaitCurator struct {
	*memory.Store
	committed chan struct{}
	calls     atomic.Int64
}

func (s *CommitThenWaitCurator) CurateFact(ctx context.Context, agent, id, action, text string, expected memory.Fact) (memory.Fact, error) {
	s.calls.Add(1)
	fact, err := s.Store.CurateFact(ctx, agent, id, action, text, expected)
	if err != nil {
		return fact, err
	}
	close(s.committed)
	<-ctx.Done()
	return fact, ctx.Err()
}

func TestInspectionCancellationAfterCommitDoesNotClaimRollbackOrReplay(t *testing.T) {
	store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir(), StrictWrite: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fact, err := store.PutReturningFact(context.Background(), "a", "old policy")
	if err != nil {
		t.Fatal(err)
	}
	backend := &CommitThenWaitCurator{Store: store, committed: make(chan struct{})}
	server := NewServer(backend, nil)
	defer server.cancelInspections()
	client := pipeInspectionClient(t, server)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := client.CurateFact(ctx, "a", fact.ID, "revise", "new policy", fact); done <- err }()
	awaitInspectionSignal(t, backend.committed, "committed revision")
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, memory.ErrMutationOutcomeUnknown) {
			t.Fatalf("claimed definite mutation outcome: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled mutation did not return")
	}
	awaitInspectionCleanup(t, server)
	page, err := store.ListFacts(context.Background(), "a", "all", "", "", 10)
	if err != nil || page.Total != 2 || backend.calls.Load() != 1 {
		t.Fatalf("committed revision was replayed/lost: %+v calls=%d err=%v", page, backend.calls.Load(), err)
	}
}

func TestInspectionRegistryBoundsCancellationRaceAndCleanup(t *testing.T) {
	server := NewServer(nil, nil)
	t.Cleanup(server.cancelInspections)
	meta := InspectionContext{RequestID: "0123456789abcdef0123456789abcdef", DeadlineUnixNano: time.Now().Add(time.Second).UnixNano()}
	if err := server.CancelInspection(&CancelInspectionRequest{Context: meta}, &CancelInspectionResponse{}); err != nil {
		t.Fatal(err)
	}
	ctx, finish, err := server.beginInspection(meta)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("cancel-before-register was lost")
	}
	finish()
	for i := 0; i < maxInspectionCalls; i++ {
		meta := InspectionContext{RequestID: fmt.Sprintf("%032x", i), DeadlineUnixNano: time.Now().Add(10 * time.Second).UnixNano()}
		if err := server.CancelInspection(&CancelInspectionRequest{Context: meta}, &CancelInspectionResponse{}); err != nil {
			t.Fatal(err)
		}
	}
	extra := InspectionContext{RequestID: "ffffffffffffffffffffffffffffffff", DeadlineUnixNano: time.Now().Add(time.Second).UnixNano()}
	if err := server.CancelInspection(&CancelInspectionRequest{Context: extra}, &CancelInspectionResponse{}); err == nil {
		t.Fatal("registry was not bounded")
	}
	server.cancelInspections()
	extra.DeadlineUnixNano = time.Now().Add(40 * time.Millisecond).UnixNano()
	if err := server.CancelInspection(&CancelInspectionRequest{Context: extra}, &CancelInspectionResponse{}); err != nil {
		t.Fatal(err)
	}
	awaitInspectionCleanup(t, server)
	far := InspectionContext{RequestID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", DeadlineUnixNano: time.Now().Add(time.Hour).UnixNano()}
	ctx, finish, err = server.beginInspection(far)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	deadline, _ := ctx.Deadline()
	if time.Until(deadline) > maxInspectionDuration {
		t.Fatal("server accepted unbounded deadline")
	}
	server.cancelInspections()
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatal("server shutdown did not cancel active call")
	}
}

// blockedInspectionCodec models a transport whose writer has stopped making
// progress. net/rpc serializes all other sends behind this blocked WriteRequest.
type blockedInspectionCodec struct {
	release chan struct{}
	once    sync.Once
	writes  atomic.Int64
}

func (c *blockedInspectionCodec) WriteRequest(_ *netrpc.Request, _ any) error {
	c.writes.Add(1)
	<-c.release
	return io.ErrClosedPipe
}
func (c *blockedInspectionCodec) ReadResponseHeader(_ *netrpc.Response) error {
	<-c.release
	return io.EOF
}
func (c *blockedInspectionCodec) ReadResponseBody(_ any) error { return io.EOF }
func (c *blockedInspectionCodec) Close() error                 { c.once.Do(func() { close(c.release) }); return nil }

func TestInspectionBlockedTransportHasPerClientBoundedCalls(t *testing.T) {
	codec := &blockedInspectionCodec{release: make(chan struct{})}
	client := &Client{rpc: netrpc.NewClientWithCodec(codec), callTimeout: time.Second}
	t.Cleanup(func() { _ = client.Close() })
	// Each timeout returns to its caller, while the underlying Call stays
	// blocked and must continue holding its slot. No canceled flood can spawn
	// more than maxClientInspectionCalls workers for this client.
	client.initInspectionSlots()
	for attempts := 0; attempts < maxClientInspectionCalls*4 && len(client.inspectionSendSlots) < maxClientInspectionCalls; attempts++ {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Millisecond)
		_, err := rpcInspectionCall[PingResponse](client, ctx, "Ping", &PingRequest{}, nil, false)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, ErrInspectionBusy) {
			t.Fatalf("blocked transport result: %v", err)
		}
	}
	if n := len(client.inspectionSendSlots); n != maxClientInspectionCalls {
		t.Fatalf("pending calls=%d, want %d", n, maxClientInspectionCalls)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, err := rpcInspectionCall[CurateFactResponse](client, ctx, "CurateFact", &CurateFactRequest{}, nil, true)
	cancel()
	if !errors.Is(err, ErrInspectionBusy) || errors.Is(err, memory.ErrMutationOutcomeUnknown) || time.Since(start) > 100*time.Millisecond {
		t.Fatalf("capacity exhaustion did not fail before dispatch: %v after %s", err, time.Since(start))
	}
	meta := InspectionContext{RequestID: "0123456789abcdef0123456789abcdef", DeadlineUnixNano: time.Now().Add(time.Second).UnixNano()}
	for i := 0; i < 1000; i++ {
		client.cancelInspection(&meta)
	}
	if n := len(client.inspectionCancelSlots); n != maxClientInspectionCancels {
		t.Fatalf("pending cancels=%d, want %d", n, maxClientInspectionCancels)
	}
	if codec.writes.Load() != 1 {
		t.Fatalf("unexpected active transport writes: %d", codec.writes.Load())
	}

	// A saturated client has no effect on another client's requests or explicit
	// cancellation. Exercise a real store and server, not just separate channels.
	store, server, healthy, embedder := inspectionEmbedderServer(t)
	if _, err := store.PutReturningFact(context.Background(), "a", "database policy"); err != nil {
		t.Fatal(err)
	}
	embedder.block.Store(true)
	healthyCtx, healthyCancel := context.WithCancel(context.Background())
	defer healthyCancel()
	done := make(chan error, 1)
	go func() { _, err := healthy.RecallPreview(healthyCtx, "a", "database", 8); done <- err }()
	awaitInspectionSignal(t, embedder.started, "healthy client embedding")
	healthyCancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("other client could not cancel")
	}
	awaitInspectionSignal(t, embedder.canceled, "healthy client remote cancellation")
	awaitInspectionCleanup(t, server)
	if err := healthy.Ping(); err != nil {
		t.Fatal(err)
	}

	// Slots remain occupied until the transport Call actually finishes, then
	// are all returned. This also releases every fixture goroutine before exit.
	_ = codec.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(client.inspectionSendSlots) == 0 && len(client.inspectionCancelSlots) == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("transport completion leaked slots: sends=%d cancels=%d", len(client.inspectionSendSlots), len(client.inspectionCancelSlots))
}

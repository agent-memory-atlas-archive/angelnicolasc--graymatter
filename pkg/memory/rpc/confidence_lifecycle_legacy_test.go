package rpc

import (
	"errors"
	"net/rpc"
	"net/rpc/jsonrpc"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// LegacyLifecycleProtocol exposes the old List/UpdateFact wire shape as well
// as a new endpoint whose advertisement can be changed by a test.
type LegacyLifecycleProtocol struct {
	mu          sync.Mutex
	fact        memory.Fact
	capable     bool
	newCalls    int
	updateCalls int
	writeErr    error
}

func (s *LegacyLifecycleProtocol) Ping(_ *PingRequest, out *PingResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out.Protocol = Protocol
	if s.capable {
		out.Capabilities = []string{ConfidenceLifecycleV1}
	}
	return nil
}

func (s *LegacyLifecycleProtocol) List(_ *ListRequest, out *ListResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out.Facts = []memory.Fact{s.fact}
	return nil
}

func (s *LegacyLifecycleProtocol) UpdateFact(in *UpdateFactRequest, _ *UpdateFactResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updateCalls++
	// A failing response can follow a committed mutation. The client must not
	// issue another update after it has sent this one.
	s.fact = in.Fact
	return s.writeErr
}

func (s *LegacyLifecycleProtocol) SetPinned(_ *SetPinnedRequest, _ *LifecycleResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.newCalls++
	return s.writeErr
}

func (s *LegacyLifecycleProtocol) Retire(_ *RetireRequest, _ *LifecycleResponse) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.newCalls++
	return s.writeErr
}

func startLegacyLifecycleProtocol(t *testing.T, svc *LegacyLifecycleProtocol) *Client {
	t.Helper()
	dir := t.TempDir()
	listener, cleanup, err := Listen(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(cleanup)
	server := rpc.NewServer()
	if err := server.RegisterName(ServiceName, svc); err != nil {
		t.Fatal(err)
	}
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.ServeCodec(jsonrpc.NewServerCodec(conn))
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-stopped })
	return dialT(t, dir)
}

func TestLegacyLifecycleNegotiatesBeforeChoosingSnapshotWrites(t *testing.T) {
	for _, operation := range []string{"pin", "unpin", "forget"} {
		t.Run(operation, func(t *testing.T) {
			old := memory.Fact{ID: "id", AgentID: "a", Text: "decision", Confidence: "inferred"}
			fresh := old
			fresh.Confidence, fresh.Weight, fresh.AccessCount = "verified", 0.75, 42
			fresh.Pinned, fresh.PinnedAt = true, time.Unix(100, 0).UTC()
			svc := &LegacyLifecycleProtocol{fact: fresh}
			client := startLegacyLifecycleProtocol(t, svc)
			var err error
			if operation == "forget" {
				err = client.Retire("a", old)
			} else {
				err = client.SetPinned("a", operation == "pin", old)
			}
			if err != nil {
				t.Fatal(err)
			}
			svc.mu.Lock()
			defer svc.mu.Unlock()
			actual := svc.fact
			if svc.newCalls != 0 || svc.updateCalls != 1 {
				t.Fatalf("wrong negotiated route: new=%d legacy=%d", svc.newCalls, svc.updateCalls)
			}
			switch operation {
			case "forget":
				fresh.SupersededBy = memory.SupersededByAgent
			case "pin":
				if actual.PinnedAt.IsZero() {
					t.Fatal("pin timestamp missing")
				}
				fresh.PinnedAt = actual.PinnedAt
			case "unpin":
				fresh.Pinned, fresh.PinnedAt = false, time.Time{}
			}
			if !reflect.DeepEqual(actual, fresh) {
				t.Fatalf("legacy patch overwrote unrelated fresh metadata: got=%+v want=%+v", actual, fresh)
			}
		})
	}
}

func TestLegacyLifecycleRejectsChangedTargetsBeforeWrites(t *testing.T) {
	for _, change := range []string{"identity", "text", "kind", "retired"} {
		t.Run(change, func(t *testing.T) {
			old := memory.Fact{ID: "id", AgentID: "a", Text: "decision"}
			fresh := old
			switch change {
			case "identity":
				fresh.ID = "another-id"
			case "text":
				fresh.Text = "replacement"
			case "kind":
				fresh.Kind = memory.KindAlias
			case "retired":
				fresh.SupersededBy = memory.SupersededByAgent
			}
			svc := &LegacyLifecycleProtocol{fact: fresh}
			client := startLegacyLifecycleProtocol(t, svc)
			if err := client.SetPinned("a", true, old); !errors.Is(err, memory.ErrFactChanged) {
				t.Fatalf("changed target error=%v", err)
			}
			if err := client.Retire("a", old); !errors.Is(err, memory.ErrFactChanged) {
				t.Fatalf("changed target retirement error=%v", err)
			}
			svc.mu.Lock()
			defer svc.mu.Unlock()
			if svc.updateCalls != 0 || svc.newCalls != 0 {
				t.Fatal("invalid target reached a write")
			}
		})
	}
}

func TestLifecycleNeverFallsBackAfterMutationDispatch(t *testing.T) {
	for _, capable := range []bool{false, true} {
		for _, operation := range []string{"pin", "forget"} {
			for _, failure := range []error{errors.New("response failed after commit"), ErrUnsupportedCapability} {
				t.Run(operation+map[bool]string{false: "/legacy/", true: "/capable/"}[capable]+failure.Error(), func(t *testing.T) {
					fact := memory.Fact{ID: "id", AgentID: "a", Text: "decision"}
					svc := &LegacyLifecycleProtocol{fact: fact, capable: capable, writeErr: failure}
					client := startLegacyLifecycleProtocol(t, svc)
					var err error
					if operation == "pin" {
						err = client.SetPinned("a", true, fact)
					} else {
						err = client.Retire("a", fact)
					}
					if err == nil {
						t.Fatal("mutation failure hidden")
					}
					svc.mu.Lock()
					defer svc.mu.Unlock()
					if svc.newCalls+svc.updateCalls != 1 || (capable && svc.updateCalls != 0) || (!capable && svc.newCalls != 0) {
						t.Fatalf("mutation retried or route changed: new=%d legacy=%d", svc.newCalls, svc.updateCalls)
					}
				})
			}
		}
	}
}

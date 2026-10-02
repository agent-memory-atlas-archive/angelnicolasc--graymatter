package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/audit"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// tuiStoreCapabilities is additive so old CLI integrations and store fakes
// remain valid. An unsupported inspection must never become an ordinary recall.
type tuiStoreCapabilities interface {
	InspectRecall(context.Context, string, string, int) ([]memory.RecallReceipt, error)
	ListFacts(context.Context, string, string, string, string, int) (memory.FactPage, error)
	CurateFact(context.Context, string, string, string, string, memory.Fact) (memory.Fact, error)
}

// openTUIStore enforces read-only at the client boundary, without changing the
// daemon's policy for other clients or silently switching to a direct handle.
func openTUIStore(readOnly bool) (cliStore, error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if readOnly {
		info, err := os.Stat(filepath.Join(dir, "gray.db"))
		if err != nil {
			return nil, fmt.Errorf("read-only inspection requires an existing store: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("read-only inspection requires a regular gray.db file")
		}
	}
	var store cliStore
	if readOnly && (noDaemon || os.Getenv("GRAYMATTER_NO_DAEMON") == "1") {
		cfg := graymatter.DefaultConfig()
		cfg.DataDir, cfg.ReadOnly = dir, true
		mem, openErr := graymatter.NewWithConfig(cfg)
		if openErr != nil {
			return nil, openErr
		}
		store = &directStore{mem: mem, store: mem.Advanced()}
	} else if readOnly {
		connect := func() (cliStore, error) {
			client, err := daemon.ConnectNoSpawn(dir)
			if err != nil {
				return nil, fmt.Errorf("read-only inspection needs a running daemon (or explicit --no-daemon): %w", err)
			}
			return daemonStore{Client: client}, nil
		}
		initial, err := connect()
		if err != nil {
			return nil, err
		}
		store = newReconnectingStoreAt(initial, connect)
	} else {
		store, err = openStoreAt(dir)
		if err != nil {
			return nil, err
		}
	}
	if readOnly || store.IsReadOnly() {
		return &readOnlyTUIStore{cliStore: store}, nil
	}
	return store, nil
}

func (d *directStore) InspectRecall(ctx context.Context, agent, query string, topK int) ([]memory.RecallReceipt, error) {
	s, ok := d.store.(memory.Inspector)
	if !ok {
		return nil, memory.ErrInspectionUnsupported
	}
	if topK <= 0 {
		topK = d.mem.Config().TopK
	}
	return s.RecallPreview(ctx, agent, query, topK)
}
func (d *directStore) ListFacts(ctx context.Context, agent, state, query, cursor string, limit int) (memory.FactPage, error) {
	s, ok := d.store.(memory.Inspector)
	if !ok {
		return memory.FactPage{}, memory.ErrInspectionUnsupported
	}
	return s.ListFacts(ctx, agent, state, query, cursor, limit)
}
func (d *directStore) CurateFact(ctx context.Context, agent, id, action, text string, expected memory.Fact) (memory.Fact, error) {
	s, ok := d.store.(memory.FactCurator)
	if !ok {
		return memory.Fact{}, memory.ErrInspectionUnsupported
	}
	return s.CurateFact(ctx, agent, id, action, text, expected)
}
func (d *directStore) InspectHealth(ctx context.Context) (memory.InspectionHealth, error) {
	s, ok := d.store.(interface {
		InspectHealth(context.Context) (memory.InspectionHealth, error)
	})
	if !ok {
		return memory.InspectionHealth{}, memory.ErrInspectionUnsupported
	}
	return s.InspectHealth(ctx)
}

func (r *reconnectingStore) InspectRecall(ctx context.Context, agent, query string, topK int) ([]memory.RecallReceipt, error) {
	var out []memory.RecallReceipt
	err := r.do(func(s cliStore) error {
		cap, ok := s.(interface {
			InspectRecall(context.Context, string, string, int) ([]memory.RecallReceipt, error)
		})
		if !ok {
			return memory.ErrInspectionUnsupported
		}
		var err error
		out, err = cap.InspectRecall(ctx, agent, query, topK)
		return err
	})
	return out, err
}
func (r *reconnectingStore) ListFacts(ctx context.Context, agent, state, query, cursor string, limit int) (memory.FactPage, error) {
	var out memory.FactPage
	err := r.do(func(s cliStore) error {
		cap, ok := s.(interface {
			ListFacts(context.Context, string, string, string, string, int) (memory.FactPage, error)
		})
		if !ok {
			return memory.ErrInspectionUnsupported
		}
		var err error
		out, err = cap.ListFacts(ctx, agent, state, query, cursor, limit)
		return err
	})
	return out, err
}
func (r *reconnectingStore) InspectHealth(ctx context.Context) (memory.InspectionHealth, error) {
	var out memory.InspectionHealth
	err := r.do(func(s cliStore) error {
		cap, ok := s.(interface {
			InspectHealth(context.Context) (memory.InspectionHealth, error)
		})
		if !ok {
			return memory.ErrInspectionUnsupported
		}
		var err error
		out, err = cap.InspectHealth(ctx)
		return err
	})
	return out, err
}
func (r *reconnectingStore) CurateFact(ctx context.Context, agent, id, action, text string, expected memory.Fact) (memory.Fact, error) {
	if err := ctx.Err(); err != nil {
		return memory.Fact{}, err
	}
	var f memory.Fact
	err := r.mutate(func(store cliStore) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		s, ok := store.(memory.FactCurator)
		if !ok {
			return memory.ErrInspectionUnsupported
		}
		var err error
		f, err = s.CurateFact(ctx, agent, id, action, text, expected)
		return err
	})
	return f, err
}

// The base interface is embedded only to forward read methods. Every mutator
// on cliStore is explicitly blocked, and optional mutation capabilities are
// intentionally not promoted from the underlying concrete implementation.
type readOnlyTUIStore struct{ cliStore }

func (*readOnlyTUIStore) IsReadOnly() bool { return true }
func (*readOnlyTUIStore) Remember(context.Context, string, string) error {
	return memory.ErrStoreReadOnly
}
func (*readOnlyTUIStore) PutShared(context.Context, string) error { return memory.ErrStoreReadOnly }
func (*readOnlyTUIStore) PutReturningFact(context.Context, string, string) (memory.Fact, error) {
	return memory.Fact{}, memory.ErrStoreReadOnly
}
func (*readOnlyTUIStore) PutAlias(context.Context, string, string, []string) error {
	return memory.ErrStoreReadOnly
}
func (*readOnlyTUIStore) Delete(string, string) error               { return memory.ErrStoreReadOnly }
func (*readOnlyTUIStore) UpdateFact(string, memory.Fact) error      { return memory.ErrStoreReadOnly }
func (*readOnlyTUIStore) Consolidate(context.Context, string) error { return memory.ErrStoreReadOnly }
func (*readOnlyTUIStore) CheckpointSave(session.Checkpoint) (session.Checkpoint, error) {
	return session.Checkpoint{}, memory.ErrStoreReadOnly
}
func (*readOnlyTUIStore) SessionKill(string) error                 { return memory.ErrStoreReadOnly }
func (*readOnlyTUIStore) SessionSave(harness.HarnessSession) error { return memory.ErrStoreReadOnly }
func (*readOnlyTUIStore) KGLink(string, string, string) error      { return memory.ErrStoreReadOnly }
func (*readOnlyTUIStore) ExportGraphObsidian(string) error         { return memory.ErrStoreReadOnly }
func (*readOnlyTUIStore) AuditWrite(audit.Entry) error             { return memory.ErrStoreReadOnly }
func (*readOnlyTUIStore) TokenRecord(string, string, uint64, uint64, uint64, uint64) error {
	return memory.ErrStoreReadOnly
}
func (*readOnlyTUIStore) CurateFact(context.Context, string, string, string, string, memory.Fact) (memory.Fact, error) {
	return memory.Fact{}, memory.ErrStoreReadOnly
}

func (r *readOnlyTUIStore) InspectRecall(ctx context.Context, agent, query string, topK int) ([]memory.RecallReceipt, error) {
	s, ok := r.cliStore.(interface {
		InspectRecall(context.Context, string, string, int) ([]memory.RecallReceipt, error)
	})
	if !ok {
		return nil, memory.ErrInspectionUnsupported
	}
	return s.InspectRecall(ctx, agent, query, topK)
}
func (r *readOnlyTUIStore) ListFacts(ctx context.Context, agent, state, query, cursor string, limit int) (memory.FactPage, error) {
	s, ok := r.cliStore.(interface {
		ListFacts(context.Context, string, string, string, string, int) (memory.FactPage, error)
	})
	if !ok {
		return memory.FactPage{}, memory.ErrInspectionUnsupported
	}
	return s.ListFacts(ctx, agent, state, query, cursor, limit)
}
func (r *readOnlyTUIStore) InspectHealth(ctx context.Context) (memory.InspectionHealth, error) {
	s, ok := r.cliStore.(interface {
		InspectHealth(context.Context) (memory.InspectionHealth, error)
	})
	if !ok {
		return memory.InspectionHealth{}, memory.ErrInspectionUnsupported
	}
	return s.InspectHealth(ctx)
}
func (r *readOnlyTUIStore) RecallExplain(ctx context.Context, agent, query string, topK int) ([]memory.RecallReceipt, error) {
	return r.InspectRecall(ctx, agent, query, topK)
}
func (r *readOnlyTUIStore) Recall(ctx context.Context, agent, query string, topK int) ([]string, error) {
	receipts, err := r.InspectRecall(ctx, agent, query, topK)
	if err != nil {
		return nil, err
	}
	texts := make([]string, len(receipts))
	for i, receipt := range receipts {
		texts[i] = receipt.Text
	}
	return texts, nil
}
func (r *readOnlyTUIStore) RecallDefault(ctx context.Context, agent, query string) ([]string, error) {
	return r.Recall(ctx, agent, query, 0)
}
func (r *readOnlyTUIStore) RecallShared(ctx context.Context, query string, topK int) ([]string, error) {
	return r.Recall(ctx, memory.SharedAgentID, query, topK)
}
func (r *readOnlyTUIStore) RecallDetailed(ctx context.Context, agent, query string, topK int) ([]string, string, error) {
	texts, err := r.Recall(ctx, agent, query, topK)
	return texts, "", err
}
func (r *readOnlyTUIStore) RecallAll(context.Context, string, string, int) ([]string, error) {
	// Namespace merging has its own retrieval semantics and bookkeeping.
	// Inspection callers select a namespace explicitly instead of changing it.
	return nil, memory.ErrInspectionUnsupported
}

var (
	_ tuiStoreCapabilities = (*directStore)(nil)
	_ tuiStoreCapabilities = daemonStore{}
	_ tuiStoreCapabilities = (*reconnectingStore)(nil)
	_ tuiStoreCapabilities = (*readOnlyTUIStore)(nil)
	_ cliStore             = (*readOnlyTUIStore)(nil)
)

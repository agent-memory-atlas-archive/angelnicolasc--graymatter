package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	bolt "go.etcd.io/bbolt"
)

type doctorTreeEntry struct {
	mode   os.FileMode
	mtime  time.Time
	hash   [32]byte
	target string
}

func snapshotDoctorTree(t *testing.T, root string) map[string]doctorTreeEntry {
	t.Helper()
	result := make(map[string]doctorTreeEntry)
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		record := doctorTreeEntry{mode: info.Mode(), mtime: info.ModTime()}
		if info.Mode().IsRegular() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			record.hash = sha256.Sum256(data)
		} else if info.Mode()&os.ModeSymlink != 0 {
			record.target, err = os.Readlink(path)
			if err != nil {
				return err
			}
		}
		result[rel] = record
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func assertDoctorTreeUnchanged(t *testing.T, root string, before map[string]doctorTreeEntry) {
	t.Helper()
	if after := snapshotDoctorTree(t, root); !reflect.DeepEqual(before, after) {
		t.Fatalf("persistent tree changed during doctor\nbefore: %#v\nafter: %#v", before, after)
	}
}

// D01: the old fixed-name probe can be an arbitrary user entry. None of
// these forms may be opened, truncated, removed or replaced.
func TestDoctorD01_LegacyProbeIsUntouched(t *testing.T) {
	for _, kind := range []string{"regular", "hardlink", "directory", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "store")
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			probe := filepath.Join(dir, ".doctor_probe")
			outside := filepath.Join(root, "outside")
			if err := os.WriteFile(outside, []byte("external data survives"), 0o600); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "regular":
				err = os.WriteFile(probe, []byte("user probe"), 0o600)
			case "hardlink":
				err = os.Link(outside, probe)
			case "directory":
				err = os.Mkdir(probe, 0o700)
			case "symlink":
				err = os.Symlink(outside, probe)
			}
			if err != nil {
				if kind == "symlink" && runtime.GOOS == "windows" {
					t.Skipf("Windows fixture cannot create a symlink: %v; hardlink remains covered", err)
				}
				t.Fatal(err)
			}
			before := snapshotDoctorTree(t, root)
			if c := checkDataDir(dir); c.Status != "ok" {
				t.Fatalf("data dir: %+v", c)
			}
			if c := checkStore(dir); c.Status != "info" {
				t.Fatalf("store: %+v", c)
			}
			if c := checkKG(dir); c.Status != "info" {
				t.Fatalf("graph: %+v", c)
			}
			assertDoctorTreeUnchanged(t, root, before)
		})
	}
}

// D02: only ENOENT means absent; permission and IO errors are failures.
func TestDoctorD02_DataDirectoryErrors(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing")
	if c := checkDataDir(missing); c.Status != "warn" || !strings.Contains(c.Hint, "init --store-only") {
		t.Fatalf("missing: %+v", c)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing path was created: %v", err)
	}
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if c := checkDataDir(file); c.Status != "fail" {
		t.Fatalf("file: %+v", c)
	}
	old := doctorDataDirStat
	t.Cleanup(func() { doctorDataDirStat = old })
	for _, injected := range []error{os.ErrPermission, errors.New("injected EIO")} {
		doctorDataDirStat = func(string) (os.FileInfo, error) { return nil, injected }
		if c := checkDataDir(root); c.Status != "fail" || !strings.Contains(c.Detail, injected.Error()) {
			t.Fatalf("injected error %v: %+v", injected, c)
		}
	}
}

type doctorTestEmbedder struct{}

func (doctorTestEmbedder) Embed(context.Context, string) ([]float32, error) {
	return []float32{0.5, 0.5}, nil
}
func (doctorTestEmbedder) Dimensions() int { return 2 }
func (doctorTestEmbedder) Name() string    { return "doctor-test" }

// D03–D04: read useful counts from a real DB with vector and graph state,
// while every persistent byte and mtime (including access metadata) stays put.
func TestDoctorD03D04_ReadOnlyStoreAndGraph(t *testing.T) {
	dir := t.TempDir()
	store, err := memory.Open(memory.StoreConfig{DataDir: dir, Embedder: doctorTestEmbedder{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range []string{"alpha fact", "beta fact"} {
		if err := store.Put(context.Background(), "agent", fact); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Recall(context.Background(), "agent", "alpha", 1); err != nil {
		t.Fatal(err)
	}
	graph, err := kg.Open(store.DB())
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Upsert(kg.Node{ID: "person:alpha", Label: "Alpha", EntityType: "person"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before := snapshotDoctorTree(t, dir)
	if _, ok := before["vectors"]; !ok {
		t.Fatal("vector fixture was not created")
	}
	for i := 0; i < 2; i++ {
		c := checkStore(dir)
		if c.Status != "ok" || !strings.Contains(c.Detail, "2 fact(s)") {
			t.Fatalf("store read %d: %+v", i, c)
		}
		g := checkKG(dir)
		if g.Status != "ok" || !strings.Contains(g.Detail, "1 nodes") {
			t.Fatalf("graph read %d: %+v", i, g)
		}
	}
	assertDoctorTreeUnchanged(t, dir, before)
	v := doctorReadOnlyVectors{}
	if err := v.EnsureCollection("any"); err != nil {
		t.Fatal(err)
	}
	if err := v.AddDocument(context.Background(), "any", "id", "text", nil, nil); !errors.Is(err, memory.ErrStoreReadOnly) {
		t.Fatalf("AddDocument = %v", err)
	}
	if _, err := v.Query(context.Background(), "any", nil, 1); err == nil {
		t.Fatal("vector query unexpectedly available in setup diagnostics")
	}
}

// D03: a valid database can have facts without any vector or graph files.
// Opening it for setup diagnostics must not materialize a vector backend.
func TestDoctorD03_ValidDatabaseWithoutVectors(t *testing.T) {
	dir := t.TempDir()
	store, err := memory.Open(memory.StoreConfig{DataDir: dir, VectorBackend: doctorReadOnlyVectors{}})
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range []string{"first fact", "second fact"} {
		if err := store.Put(context.Background(), "agent", fact); err != nil {
			_ = store.Close()
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "vectors")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture already has vectors: %v", err)
	}
	before := snapshotDoctorTree(t, dir)
	if c := checkStore(dir); c.Status != "ok" || !strings.Contains(c.Detail, "2 fact(s)") {
		t.Fatalf("read-only store count: %+v", c)
	}
	if c := checkKG(dir); c.Status != "info" || !strings.Contains(c.Detail, "0 nodes / 0 edges") {
		t.Fatalf("absent graph: %+v", c)
	}
	assertDoctorTreeUnchanged(t, dir, before)
}

type doctorRecordingDaemon struct {
	calls                                                       []string
	agents                                                      []string
	stats                                                       map[string]memory.MemoryStats
	listErr, statsErr, pendingErr, overviewErr, kgErr, closeErr error
}

func (d *doctorRecordingDaemon) ListAgents() ([]string, error) {
	d.calls = append(d.calls, "ListAgents")
	return d.agents, d.listErr
}
func (d *doctorRecordingDaemon) Stats(agent string) (memory.MemoryStats, error) {
	d.calls = append(d.calls, "Stats")
	return d.stats[agent], d.statsErr
}
func (d *doctorRecordingDaemon) PendingVectorCount() (int, error) {
	d.calls = append(d.calls, "PendingVectorCount")
	return 0, d.pendingErr
}
func (d *doctorRecordingDaemon) StoreOverview() (*daemon.StoreOverviewResponse, error) {
	d.calls = append(d.calls, "StoreOverview")
	return &daemon.StoreOverviewResponse{}, d.overviewErr
}
func (d *doctorRecordingDaemon) KGState() (*daemon.KGStateResponse, error) {
	d.calls = append(d.calls, "KGState")
	return &daemon.KGStateResponse{Nodes: 2, Edges: 1}, d.kgErr
}
func (d *doctorRecordingDaemon) Close() error {
	d.calls = append(d.calls, "Close")
	return d.closeErr
}

func withDoctorDaemon(t *testing.T, connect func(string) (doctorDaemonReader, error)) {
	t.Helper()
	previous := doctorConnectNoSpawn
	doctorConnectNoSpawn = connect
	t.Cleanup(func() { doctorConnectNoSpawn = previous })
}

// D05: a live daemon receives only allowlisted read RPCs. Stale discovery
// falls back to a direct read without rewriting or removing the file.
func TestDoctorD05_DaemonAndStaleDiscovery(t *testing.T) {
	t.Run("live daemon allowlist", func(t *testing.T) {
		dir := t.TempDir()
		seedGrayDB(t, dir, 1)
		before := snapshotDoctorTree(t, dir)
		var connections []*doctorRecordingDaemon
		withDoctorDaemon(t, func(string) (doctorDaemonReader, error) {
			d := &doctorRecordingDaemon{agents: []string{"doc-agent"}, stats: map[string]memory.MemoryStats{"doc-agent": {FactCount: 1}}}
			connections = append(connections, d)
			return d, nil
		})
		if c := checkStore(dir); c.Status != "ok" || !strings.Contains(c.Detail, "1 fact(s)") {
			t.Fatalf("daemon store: %+v", c)
		}
		if c := checkKG(dir); c.Status != "ok" || !strings.Contains(c.Detail, "2 nodes") {
			t.Fatalf("daemon graph: %+v", c)
		}
		if len(connections) != 2 || !reflect.DeepEqual(connections[0].calls, []string{"ListAgents", "Stats", "PendingVectorCount", "StoreOverview", "Close"}) || !reflect.DeepEqual(connections[1].calls, []string{"KGState", "Close"}) {
			t.Fatalf("unexpected daemon RPCs: %+v", connections)
		}
		assertDoctorTreeUnchanged(t, dir, before)
	})
	t.Run("stale discovery", func(t *testing.T) {
		dir := t.TempDir()
		seedGrayDB(t, dir, 1)
		if err := os.WriteFile(filepath.Join(dir, "graymatter.addr"), []byte("stale, invalid discovery"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := snapshotDoctorTree(t, dir)
		if c := checkStore(dir); c.Status != "ok" || !strings.Contains(c.Detail, "direct read") {
			t.Fatalf("direct fallback: %+v", c)
		}
		assertDoctorTreeUnchanged(t, dir, before)
	})
}

type doctorFailingStore struct {
	listErr, statsErr, closeErr error
	agents                      []string
	db                          *bolt.DB
}

func (s *doctorFailingStore) ListAgents() ([]string, error) { return s.agents, s.listErr }
func (s *doctorFailingStore) Stats(string) (memory.MemoryStats, error) {
	return memory.MemoryStats{}, s.statsErr
}
func (s *doctorFailingStore) DB() *bolt.DB { return s.db }
func (s *doctorFailingStore) Close() error {
	if s.db != nil {
		if err := s.db.Close(); err != nil {
			return err
		}
	}
	return s.closeErr
}

// D07: no partial totals are certified; direct failures fail, daemon RPC
// failures warn, and Close errors are observed.
func TestDoctorD07_ReadFailuresAreVisible(t *testing.T) {
	dir := t.TempDir()
	seedGrayDB(t, dir, 1)
	withDoctorDaemon(t, func(string) (doctorDaemonReader, error) { return nil, errors.New("offline") })
	oldOpen := doctorOpenReadOnlyStore
	t.Cleanup(func() { doctorOpenReadOnlyStore = oldOpen })
	for _, tc := range []struct {
		name  string
		store *doctorFailingStore
	}{
		{"list", &doctorFailingStore{listErr: errors.New("list EIO")}},
		{"stats", &doctorFailingStore{agents: []string{"agent"}, statsErr: errors.New("stats EIO")}},
		{"close", &doctorFailingStore{listErr: errors.New("list EIO"), closeErr: errors.New("close EIO")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doctorOpenReadOnlyStore = func(string) (doctorStoreReader, error) { return tc.store, nil }
			c := checkStore(dir)
			if c.Status != "fail" || strings.Contains(c.Detail, "0 fact(s)") {
				t.Fatalf("direct failure misreported: %+v", c)
			}
		})
	}
	ro, err := bolt.Open(filepath.Join(dir, "gray.db"), 0o600, &bolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	doctorOpenReadOnlyStore = func(string) (doctorStoreReader, error) {
		return &doctorFailingStore{db: ro, closeErr: errors.New("close EIO")}, nil
	}
	if c := checkStore(dir); c.Status != "fail" || !strings.Contains(c.Detail, "close EIO") {
		t.Fatalf("direct close failure: %+v", c)
	}
	doctorOpenReadOnlyStore = oldOpen
	d := &doctorRecordingDaemon{agents: []string{"agent"}, statsErr: errors.New("rpc failed")}
	doctorConnectNoSpawn = func(string) (doctorDaemonReader, error) { return d, nil }
	if c := checkStore(dir); c.Status != "warn" || !strings.Contains(c.Detail, "incomplete") || strings.Contains(c.Detail, "0 fact(s)") {
		t.Fatalf("daemon partial total: %+v", c)
	}
	d.kgErr = errors.New("graph RPC failed")
	if c := checkKG(dir); c.Status != "warn" || !strings.Contains(c.Detail, "incomplete") {
		t.Fatalf("daemon graph RPC failure: %+v", c)
	}
}

func TestDoctorD07_DatabaseLeafTypesAndStatFailures(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(root, "outside.db")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		create func(string) error
		status string
	}{
		{"directory", func(p string) error { return os.Mkdir(p, 0o700) }, "fail"},
		{"symlink", func(p string) error { return os.Symlink(outside, p) }, "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(root, tc.name)
			if err := os.Mkdir(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := tc.create(filepath.Join(dir, "gray.db")); err != nil {
				if tc.name == "symlink" && runtime.GOOS == "windows" {
					t.Skipf("Windows fixture cannot create a symlink: %v; direct Lstat classification is also covered", err)
				}
				t.Fatal(err)
			}
			before := snapshotDoctorTree(t, root)
			if c := checkStore(dir); c.Status != tc.status {
				t.Fatalf("store: %+v", c)
			}
			if c := checkKG(dir); c.Status != tc.status {
				t.Fatalf("graph: %+v", c)
			}
			assertDoctorTreeUnchanged(t, root, before)
		})
	}
	dir := filepath.Join(root, "stat-error")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	old := doctorDBLstat
	t.Cleanup(func() { doctorDBLstat = old })
	doctorDBLstat = func(string) (os.FileInfo, error) { return nil, os.ErrPermission }
	if c := checkStore(dir); c.Status != "fail" {
		t.Fatalf("DB permission: %+v", c)
	}
	if c := checkKG(dir); c.Status != "fail" {
		t.Fatalf("graph DB permission: %+v", c)
	}
}

// D07: graph listings skip invalid JSON in normal runtime paths. Doctor must
// fail the diagnostic rather than report a plausible partial or empty graph.
func TestDoctorD07_PartialAndCorruptGraph(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*bolt.Tx) error
	}{
		{"missing edges bucket", func(tx *bolt.Tx) error {
			_, err := tx.CreateBucket([]byte("kg_nodes"))
			return err
		}},
		{"corrupt node", func(tx *bolt.Tx) error {
			nodes, err := tx.CreateBucket([]byte("kg_nodes"))
			if err != nil {
				return err
			}
			if _, err := tx.CreateBucket([]byte("kg_edges")); err != nil {
				return err
			}
			return nodes.Put([]byte("node"), []byte("{"))
		}},
		{"corrupt edge", func(tx *bolt.Tx) error {
			if _, err := tx.CreateBucket([]byte("kg_nodes")); err != nil {
				return err
			}
			edges, err := tx.CreateBucket([]byte("kg_edges"))
			if err != nil {
				return err
			}
			return edges.Put([]byte("edge"), []byte("{"))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			seedGrayDB(t, dir, 1)
			db, err := bolt.Open(filepath.Join(dir, "gray.db"), 0o600, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := db.Update(tc.prepare); err != nil {
				_ = db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			before := snapshotDoctorTree(t, dir)
			if c := checkKG(dir); c.Status != "fail" || !strings.Contains(c.Detail, "graph inspection failed") {
				t.Fatalf("graph failure was hidden: %+v", c)
			}
			assertDoctorTreeUnchanged(t, dir, before)
		})
	}
}

// D08: independent setup diagnostics can read the same store concurrently,
// including a pre-existing probe alias, without changing persistent state.
func TestDoctorD08_ConcurrentReaders(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "store")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	seedGrayDB(t, dir, 2)
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(dir, ".doctor_probe")); err != nil {
		t.Fatal(err)
	}
	before := snapshotDoctorTree(t, root)
	var wg sync.WaitGroup
	errors := make(chan string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if c := checkStore(dir); c.Status != "ok" {
				errors <- "store: " + c.Detail
				return
			}
			if c := checkKG(dir); c.Status != "info" {
				errors <- "graph: " + c.Detail
			}
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		t.Error(err)
	}
	assertDoctorTreeUnchanged(t, root, before)
}

func TestDoctorD08_ProbeReplacementDuringDiagnostics(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "store")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	seedGrayDB(t, dir, 1)
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("outside remains intact"), 0o600); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(dir, ".doctor_probe")
	if err := os.Link(outside, probe); err != nil {
		t.Fatal(err)
	}
	beforeOutside := issue81SnapshotFile(t, outside)
	beforeDB := issue81SnapshotFile(t, filepath.Join(dir, "gray.db"))
	var wg sync.WaitGroup
	issues := make(chan string, 16)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 6; j++ {
				if c := checkStore(dir); c.Status != "ok" {
					issues <- "store: " + c.Detail
					return
				}
				if c := checkKG(dir); c.Status != "info" {
					issues <- "graph: " + c.Detail
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 12; i++ {
			if err := os.Remove(probe); err != nil {
				issues <- err.Error()
				return
			}
			if err := os.Link(outside, probe); err != nil {
				issues <- err.Error()
				return
			}
		}
	}()
	wg.Wait()
	close(issues)
	for issue := range issues {
		t.Error(issue)
	}
	if got := issue81SnapshotFile(t, outside); !reflect.DeepEqual(got, beforeOutside) {
		t.Fatalf("outside target changed: before=%+v after=%+v", beforeOutside, got)
	}
	if got := issue81SnapshotFile(t, filepath.Join(dir, "gray.db")); !reflect.DeepEqual(got, beforeDB) {
		t.Fatalf("database changed: before=%+v after=%+v", beforeDB, got)
	}
}

// D06–D07: direct lock contention is incomplete, while invalid DB bytes are
// a failure; neither case creates a second writable store or vectors tree.
func TestDoctorD06D07_LockAndCorruption(t *testing.T) {
	t.Run("direct writer lock", func(t *testing.T) {
		dir := t.TempDir()
		store, err := memory.Open(memory.StoreConfig{DataDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = store.Close() }()
		before := snapshotDoctorTree(t, dir)
		start := time.Now()
		if c := checkStore(dir); c.Status != "warn" || !strings.Contains(c.Detail, "non-daemon") {
			t.Fatalf("lock: %+v", c)
		}
		if elapsed := time.Since(start); elapsed > 4*time.Second {
			t.Fatalf("lock diagnosis took %s, expected bounded bbolt timeout", elapsed)
		}
		assertDoctorTreeUnchanged(t, dir, before)
	})
	t.Run("corrupt database", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "gray.db"), []byte("not a bbolt store"), 0o600); err != nil {
			t.Fatal(err)
		}
		before := snapshotDoctorTree(t, dir)
		if c := checkStore(dir); c.Status != "fail" {
			t.Fatalf("store: %+v", c)
		}
		if c := checkKG(dir); c.Status != "fail" {
			t.Fatalf("graph: %+v", c)
		}
		assertDoctorTreeUnchanged(t, dir, before)
	})
	t.Run("malformed fact record", func(t *testing.T) {
		dir := t.TempDir()
		seedGrayDB(t, dir, 1)
		db, err := bolt.Open(filepath.Join(dir, "gray.db"), 0o600, nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte("facts")).Bucket([]byte("doc-agent")).Put([]byte("corrupt"), []byte("{"))
		}); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		before := snapshotDoctorTree(t, dir)
		if c := checkStore(dir); c.Status != "fail" || !strings.Contains(c.Detail, "raw facts") {
			t.Fatalf("malformed fact was hidden as an exact count: %+v", c)
		}
		assertDoctorTreeUnchanged(t, dir, before)
	})
}

// D09: comparing executable identity must never execute a PATH canary.
func TestDoctorD09_PathIdentityWithoutExecution(t *testing.T) {
	dir := t.TempDir()
	name := "graymatter"
	content := []byte("#!/bin/sh\nprintf canary > \"$1\"\n")
	if runtime.GOOS == "windows" {
		name, content = "graymatter.bat", []byte("@echo off\r\necho canary > %1\r\n")
	}
	canary := filepath.Join(dir, name)
	if err := os.WriteFile(canary, content, 0o755); err != nil {
		t.Fatal(err)
	}
	old := doctorLookPath
	doctorLookPath = func(string) (string, error) { return canary, nil }
	t.Cleanup(func() { doctorLookPath = old })
	if c := checkVersion(); c.Status != "warn" || !strings.Contains(c.Detail, "version not checked") {
		t.Fatalf("different PATH executable: %+v", c)
	}
	if c := checkBinaryOnPath(); c.Status != "ok" {
		t.Fatalf("PATH observation: %+v", c)
	}
	if _, err := os.Stat(filepath.Join(dir, "--version")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canary was executed: %v", err)
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	alias := filepath.Dir(self) + string(os.PathSeparator) + "." + string(os.PathSeparator) + filepath.Base(self)
	doctorLookPath = func(string) (string, error) { return alias, nil }
	if c := checkVersion(); c.Status != "ok" {
		t.Fatalf("same executable via path alias: %+v", c)
	}
	doctorLookPath = func(string) (string, error) { return "", exec.ErrDot }
	if c := checkVersion(); c.Status != "ok" {
		t.Fatalf("ErrDot version observation: %+v", c)
	}
	if c := checkBinaryOnPath(); c.Status != "warn" || !strings.Contains(c.Detail, "current directory") {
		t.Fatalf("ErrDot PATH observation: %+v", c)
	}
}

type doctorErrorWriter struct{ err error }

func (w doctorErrorWriter) Write([]byte) (int, error) { return 0, w.err }

// D10: aggregate status, compatible ok, additive schema and writer failure.
func TestDoctorD10_ReportContract(t *testing.T) {
	for _, tc := range []struct {
		checks []checkResult
		status string
		ok     bool
	}{
		{[]checkResult{{Status: "ok"}, {Status: "info"}}, "ok", true},
		{[]checkResult{{Status: "info"}, {Status: "warn"}}, "warn", true},
		{[]checkResult{{Status: "warn"}, {Status: "fail"}}, "fail", false},
	} {
		report := newDoctorSetupReport(".graymatter", tc.checks)
		if report.Status != tc.status || report.OK != tc.ok || report.Readiness != "not_evaluated" || report.DataDirWritability != "not_tested" {
			t.Fatalf("aggregate %+v", report)
		}
		var out bytes.Buffer
		if err := writeDoctorSetupReport(&out, report, true); err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
			t.Fatal(err)
		}
		if decoded["diagnostic_mode"] != "read_only" || decoded["status"] != tc.status || decoded["ok"] != tc.ok {
			t.Fatalf("JSON: %v", decoded)
		}
		if err := writeDoctorSetupReport(doctorErrorWriter{io.ErrClosedPipe}, report, true); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("writer error: %v", err)
		}
	}
}

// D11: a word in a comment is only an observed substring reference.
func TestDoctorD11_CommentIsNotReadiness(t *testing.T) {
	root := t.TempDir()
	testHomeOverride = t.TempDir()
	t.Cleanup(func() { testHomeOverride = "" })
	if err := os.WriteFile(filepath.Join(root, ".mcp.json"), []byte(`{"note":"// graymatter is not configured"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := checkMCPWiring(root)
	if c.Status != "ok" || !strings.Contains(c.Detail, "substring check") || !strings.Contains(c.Detail, "not verified") {
		t.Fatalf("heuristic label: %+v", c)
	}
	if report := newDoctorSetupReport(root, []checkResult{c}); report.Readiness != "not_evaluated" {
		t.Fatalf("heuristic certified readiness: %+v", report)
	}
}

// D12: help distinguishes the special modes from setup observations.
func TestDoctorD12_HelpScope(t *testing.T) {
	cmd := doctorCmd()
	if !strings.Contains(cmd.Long, "does not test data directory writability") || !strings.Contains(cmd.Long, "--health") || !strings.Contains(cmd.Long, "--graph") || !strings.Contains(cmd.Long, "--audit") || !strings.Contains(cmd.Long, "--embeddings") || !strings.Contains(cmd.Long, "hooks doctor") {
		t.Fatalf("doctor help omits mode boundaries: %s", cmd.Long)
	}
}

// E06/D10/D12: use the real binary with a private home and project. Both
// success and diagnostic failure emit exactly one JSON document; store-only
// still prepares the store without consulting doctor or touching the probe.
func TestDoctorE06_RealCLIReadOnly(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "project")
	storeDir := f.registerStore(filepath.Join(project, ".graymatter"))
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(storeDir, ".doctor_probe")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("user data"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := snapshotDoctorTree(t, f.root)
	out := f.mustRun(project, "", "doctor", "--json")
	var report doctorSetupReport
	decodeSingleDoctorReport(t, out.stdout, &report)
	if !report.OK || report.Status != "warn" || report.Readiness != "not_evaluated" || report.DataDirWritability != "not_tested" {
		t.Fatalf("empty project report: %+v", report)
	}
	assertDoctorTreeUnchanged(t, f.root, before)
	canaryDir := filepath.Join(f.root, "path-canary")
	if err := os.Mkdir(canaryDir, 0o700); err != nil {
		t.Fatal(err)
	}
	canary := filepath.Join(canaryDir, "graymatter")
	markerPath := filepath.Join(f.home, "doctor-canary-executed")
	content := []byte("#!/bin/sh\nprintf touched > \"$HOME/doctor-canary-executed\"\n")
	if runtime.GOOS == "windows" {
		canary += ".bat"
		content = []byte("@echo off\r\necho touched > \"%USERPROFILE%\\doctor-canary-executed\"\r\n")
	}
	if err := os.WriteFile(canary, content, 0o755); err != nil {
		t.Fatal(err)
	}
	before = snapshotDoctorTree(t, f.root)
	out = f.runCommand(f.bin, project, "", map[string]string{"PATH": canaryDir + string(os.PathListSeparator) + os.Getenv("PATH")}, "doctor", "--json")
	if out.code != 0 {
		t.Fatalf("PATH canary doctor exit=%d stderr=%q", out.code, out.stderr)
	}
	decodeSingleDoctorReport(t, out.stdout, &report)
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("PATH canary executed: %v", err)
	}
	assertDoctorTreeUnchanged(t, f.root, before)

	if err := os.WriteFile(filepath.Join(storeDir, "gray.db"), []byte("corrupt database"), 0o600); err != nil {
		t.Fatal(err)
	}
	before = snapshotDoctorTree(t, f.root)
	out = f.run(project, "", "doctor", "--json")
	if out.code != 1 {
		t.Fatalf("corrupt store exit=%d err=%v stderr=%q", out.code, out.err, out.stderr)
	}
	decodeSingleDoctorReport(t, out.stdout, &report)
	if report.OK || report.Status != "fail" || report.Readiness != "not_evaluated" {
		t.Fatalf("corrupt store report: %+v", report)
	}
	assertDoctorTreeUnchanged(t, f.root, before)

	if err := os.Remove(filepath.Join(storeDir, "gray.db")); err != nil {
		t.Fatal(err)
	}
	out = f.mustRun(project, "", "init", "--store-only")
	if !strings.Contains(out.stdout, "prepared") && !strings.Contains(out.stdout, "already") {
		t.Fatalf("store-only output unexpected: %q", out.stdout)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "user data" {
		t.Fatalf("store-only touched probe: data=%q err=%v", data, err)
	}
}

func decodeSingleDoctorReport(t *testing.T, output string, report *doctorSetupReport) {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(output))
	if err := decoder.Decode(report); err != nil {
		t.Fatalf("decode doctor JSON: %v; output %q", err, output)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("doctor emitted trailing JSON/data: %v; output %q", err, output)
	}
}

package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRuntimeStoreGatePreparationAndDatabaseLeaf(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "missing")
	if _, err := inspectRuntimeStore(missing, false); err != nil {
		t.Fatalf("eager missing store: %v", err)
	}
	if _, err := inspectRuntimeStore(missing, true); !errors.Is(err, errRuntimeStoreUnprepared) {
		t.Fatalf("guarded missing store: %v", err)
	}

	dir := filepath.Join(base, "store")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectRuntimeStore(dir, true); !errors.Is(err, errRuntimeStoreUnprepared) {
		t.Fatalf("guarded empty store: %v", err)
	}
	marker := filepath.Join(dir, "MEMORY.md")
	if err := os.WriteFile(marker, []byte("custom marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := inspectRuntimeStore(dir, true)
	if err != nil || got.preparedBy != "MEMORY.md" {
		t.Fatalf("marker-only store: %+v, %v", got, err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	db := filepath.Join(dir, "gray.db")
	if err := os.WriteFile(db, []byte("not a healthy database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(marker, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err = inspectRuntimeStore(dir, true)
	if err != nil || got.preparedBy != "gray.db" {
		t.Fatalf("regular DB must win over invalid marker without opening DB: %+v, %v", got, err)
	}
	if err := os.Remove(db); err != nil {
		t.Fatal(err)
	}
	for _, guarded := range []bool{false, true} {
		_, err = inspectRuntimeStore(dir, guarded)
		if guarded && (err == nil || !strings.Contains(err.Error(), "MEMORY.md is not a regular file")) {
			t.Fatalf("guarded invalid marker: %v", err)
		}
		if !guarded && err != nil {
			t.Fatalf("eager mode should inspect only DB leaf: %v", err)
		}
	}
}

func TestRuntimeStoreGateRejectsAliasedDatabaseWithoutTouchingTarget(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "store")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(base, "outside-db")
	const original = "external content"
	if err := os.WriteFile(target, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "gray.db")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("creating a symlink requires host permission: %v", err)
		}
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, guarded := range []bool{false, true} {
		if _, err := inspectRuntimeStore(dir, guarded); err == nil || !strings.Contains(err.Error(), "gray.db is not a regular file") {
			t.Fatalf("guarded=%v accepted aliased DB: %v", guarded, err)
		}
	}
	contents, err := os.ReadFile(target)
	if err != nil || string(contents) != original {
		t.Fatalf("external target changed: %q, %v", contents, err)
	}
}

func TestRuntimeStoreGateRejectsInspectionError(t *testing.T) {
	dir := t.TempDir()
	_, err := inspectRuntimeStoreWithLstat(dir, false, func(_ *os.Root, name string) (fs.FileInfo, error) {
		if name == "gray.db" {
			return nil, fs.ErrPermission
		}
		return nil, fs.ErrNotExist
	})
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("database inspection error was ignored: %v", err)
	}
	_, err = inspectRuntimeStoreWithLstat(dir, true, func(_ *os.Root, name string) (fs.FileInfo, error) {
		if name == "MEMORY.md" {
			return nil, fs.ErrPermission
		}
		return nil, fs.ErrNotExist
	})
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("marker inspection error was ignored: %v", err)
	}
}

func TestRuntimeStoreGateRejectsSpecialDatabaseModes(t *testing.T) {
	dir := t.TempDir()
	placeholder := filepath.Join(dir, "placeholder")
	if err := os.WriteFile(placeholder, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	regular, err := os.Lstat(placeholder)
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []fs.FileMode{fs.ModeNamedPipe, fs.ModeSocket, fs.ModeDevice} {
		for _, guarded := range []bool{false, true} {
			_, err := inspectRuntimeStoreWithLstat(dir, guarded, func(_ *os.Root, name string) (fs.FileInfo, error) {
				if name != "gray.db" {
					t.Fatalf("database type rejection inspected %s", name)
				}
				return storeOnlyModeInfo{FileInfo: regular, mode: mode}, nil
			})
			if err == nil || !strings.Contains(err.Error(), "gray.db is not a regular file") {
				t.Fatalf("mode=%s guarded=%v accepted: %v", mode, guarded, err)
			}
		}
	}
}

func TestLegacyStoreEvidenceDistinguishesEmptyFromAmbiguous(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "missing")
	if found, err := storeEvidence(missing); found || err != nil {
		t.Fatalf("missing candidate: %v, %v", found, err)
	}
	dir := filepath.Join(base, "store")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if found, err := storeEvidence(dir); found || err != nil {
		t.Fatalf("empty candidate: %v, %v", found, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	if found, err := storeEvidence(dir); !found || err != nil {
		t.Fatalf("prepared candidate: %v, %v", found, err)
	}
	if err := os.Remove(filepath.Join(dir, "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "gray.db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if found, err := storeEvidence(dir); found || err == nil {
		t.Fatalf("ambiguous candidate: %v, %v", found, err)
	}
}

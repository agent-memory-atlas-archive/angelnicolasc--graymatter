package main

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func assertStoreOnlyTree(t *testing.T, dir string, want []string) {
	t.Helper()
	var got []string
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path != dir {
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			got = append(got, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tree = %q, want %q", got, want)
	}
}

func assertNoStoreTemp(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".init-store-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("initialization temporaries left behind: %q", matches)
	}
}

func TestStoreOnlyPrepareFreshAndIdempotent(t *testing.T) {
	base := t.TempDir()
	data := filepath.Join(base, "project ü with spaces", "nested", ".graymatter")
	first, err := prepareStoreDirectory(data)
	if err != nil {
		t.Fatal(err)
	}
	if first != (storePreparationResult{status: "created", marker: "MEMORY.md"}) {
		t.Fatalf("first result = %+v", first)
	}
	marker := filepath.Join(data, "MEMORY.md")
	before, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != storeInitializationMarker {
		t.Fatalf("marker content = %q", before)
	}
	beforeInfo, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	assertStoreOnlyTree(t, data, []string{"MEMORY.md"})
	second, err := prepareStoreDirectory(data)
	if err != nil {
		t.Fatal(err)
	}
	if second != (storePreparationResult{status: "already_prepared", marker: "MEMORY.md"}) {
		t.Fatalf("second result = %+v", second)
	}
	afterInfo, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !beforeInfo.ModTime().Equal(afterInfo.ModTime()) {
		t.Fatal("idempotent run changed marker bytes or mtime")
	}
	assertStoreOnlyTree(t, data, []string{"MEMORY.md"})
}

func TestStoreOnlyExistingRegularLeavesAreUntouched(t *testing.T) {
	for _, tc := range []struct {
		name   string
		leaf   string
		marker string
		data   string
	}{
		{"corrupt database", "gray.db", "gray.db", "not a database"},
		{"zero length database", "gray.db", "gray.db", ""},
		{"custom marker", "MEMORY.md", "MEMORY.md", "personal content"},
		{"zero length marker", "MEMORY.md", "MEMORY.md", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			leaf := filepath.Join(dir, tc.leaf)
			if err := os.WriteFile(leaf, []byte(tc.data), 0o444); err != nil {
				t.Fatal(err)
			}
			other := filepath.Join(t.TempDir(), "other-link")
			if err := os.Link(leaf, other); err != nil {
				t.Fatal(err)
			}
			before, err := os.Stat(leaf)
			if err != nil {
				t.Fatal(err)
			}
			result, err := prepareStoreDirectory(dir)
			if err != nil {
				t.Fatal(err)
			}
			if result != (storePreparationResult{status: "already_prepared", marker: tc.marker}) {
				t.Fatalf("result = %+v", result)
			}
			after, err := os.Stat(leaf)
			if err != nil {
				t.Fatal(err)
			}
			contents, err := os.ReadFile(other)
			if err != nil {
				t.Fatal(err)
			}
			if string(contents) != tc.data || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("existing file or its hardlink was modified")
			}
			assertNoStoreTemp(t, dir)
			if tc.leaf == "gray.db" {
				if _, err := os.Lstat(filepath.Join(dir, "MEMORY.md")); !errors.Is(err, fs.ErrNotExist) {
					t.Fatalf("database-only store gained marker: %v", err)
				}
			}
		})
	}
}

func TestStoreOnlyDatabaseWinsWhenBothLeavesAreRegular(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"gray.db", "MEMORY.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	result, err := prepareStoreDirectory(dir)
	if err != nil || result != (storePreparationResult{status: "already_prepared", marker: "gray.db"}) {
		t.Fatalf("both regular result = %+v, %v", result, err)
	}
}

func TestStoreOnlyUnrelatedFilesDoNotPrepareStore(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"graymatter.log", ".init-store-old.tmp"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "vectors"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := prepareStoreDirectory(dir)
	if err != nil || result.status != "created" {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	assertStoreOnlyTree(t, dir, []string{".init-store-old.tmp", "MEMORY.md", "graymatter.log", "vectors"})
}

func TestStoreOnlyPreparationORAndInvalidLeaves(t *testing.T) {
	for _, tc := range []struct {
		name        string
		regularLeaf string
		badLeaf     string
		badErr      error
		wantMarker  string
		wantError   bool
	}{
		{"database overrides inspection error", "gray.db", "MEMORY.md", fs.ErrPermission, "gray.db", false},
		{"marker overrides inspection error", "MEMORY.md", "gray.db", fs.ErrPermission, "MEMORY.md", false},
		{"database overrides invalid", "gray.db", "MEMORY.md", nil, "gray.db", false},
		{"inspection error blocks creation", "", "gray.db", fs.ErrPermission, "", true},
		{"invalid blocks creation", "", "MEMORY.md", nil, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.regularLeaf != "" {
				if err := os.WriteFile(filepath.Join(dir, tc.regularLeaf), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.badErr == nil {
				if err := os.Mkdir(filepath.Join(dir, tc.badLeaf), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			ops := defaultStoreInitOps()
			lstat := ops.lstat
			if tc.badErr != nil {
				ops.lstat = func(root *os.Root, name string) (fs.FileInfo, error) {
					if name == tc.badLeaf {
						return nil, tc.badErr
					}
					return lstat(root, name)
				}
			}
			result, err := prepareStoreDirectoryWithOps(dir, ops)
			if (err != nil) != tc.wantError || result.marker != tc.wantMarker {
				t.Fatalf("result = %+v, err = %v", result, err)
			}
			assertNoStoreTemp(t, dir)
		})
	}
}

type storeOnlyModeInfo struct {
	fs.FileInfo
	mode fs.FileMode
}

func (i storeOnlyModeInfo) Mode() fs.FileMode { return i.mode }

func TestStoreOnlyRejectsSpecialLeavesWithoutOpeningThem(t *testing.T) {
	for _, mode := range []fs.FileMode{fs.ModeNamedPipe, fs.ModeSocket, fs.ModeDevice} {
		dir := t.TempDir()
		placeholder := filepath.Join(dir, "placeholder")
		if err := os.WriteFile(placeholder, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		info, err := os.Lstat(placeholder)
		if err != nil {
			t.Fatal(err)
		}
		ops := defaultStoreInitOps()
		lstat := ops.lstat
		ops.lstat = func(root *os.Root, name string) (fs.FileInfo, error) {
			if name == "MEMORY.md" {
				return storeOnlyModeInfo{FileInfo: info, mode: mode}, nil
			}
			return lstat(root, name)
		}
		ops.openFile = func(*os.Root, string, int, fs.FileMode) (*os.File, error) {
			t.Fatal("special leaf caused an open attempt")
			return nil, nil
		}
		if _, err := prepareStoreDirectoryWithOps(dir, ops); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("special mode %s accepted: %v", mode, err)
		}
	}
}

func TestStoreOnlyRejectsLeafSymlinkWithoutFollowingIt(t *testing.T) {
	dir := t.TempDir()
	external := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(external, []byte("untouched"), 0o644); err != nil {
		t.Fatal(err)
	}
	internal := filepath.Join(dir, "other")
	if err := os.WriteFile(internal, []byte("internal"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{external, internal, filepath.Join(t.TempDir(), "missing")} {
		leaf := filepath.Join(dir, "MEMORY.md")
		if err := os.Symlink(target, leaf); err != nil {
			t.Fatalf("symlink fixture unavailable: %v", err)
		}
		if _, err := prepareStoreDirectory(dir); err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Fatalf("symlink accepted: %v", err)
		}
		if err := os.Remove(leaf); err != nil {
			t.Fatal(err)
		}
	}
	contents, err := os.ReadFile(external)
	if err != nil || string(contents) != "untouched" {
		t.Fatalf("external target changed: %q, %v", contents, err)
	}
}

func TestStoreOnlyDirectoryAliasAndBrokenAlias(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatalf("directory symlink fixture unavailable: %v", err)
	}
	result, err := prepareStoreDirectory(alias)
	if err != nil || result.status != "created" {
		t.Fatalf("directory alias result = %+v, err = %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(realDir, "MEMORY.md")); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(base, "broken")
	missing := filepath.Join(base, "missing")
	if err := os.Symlink(missing, broken); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareStoreDirectory(broken); err == nil {
		t.Fatal("broken directory alias was accepted")
	}
	if _, err := os.Lstat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("broken alias target created: %v", err)
	}
	file := filepath.Join(base, "regular")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	wrongType := filepath.Join(base, "file-alias")
	if err := os.Symlink(file, wrongType); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareStoreDirectory(wrongType); err == nil {
		t.Fatal("regular file alias was accepted as data directory")
	}
	cycle := filepath.Join(base, "cycle")
	if err := os.Symlink(cycle, cycle); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareStoreDirectory(cycle); err == nil {
		t.Fatal("cyclic directory alias was accepted")
	}
}

func TestStoreOnlyTempCollisionDoesNotTruncateOccupant(t *testing.T) {
	dir := t.TempDir()
	var first, second [16]byte
	second[0] = 1
	name := ".init-store-00000000000000000000000000000000.tmp"
	if err := os.WriteFile(filepath.Join(dir, name), []byte("preserve"), 0o644); err != nil {
		t.Fatal(err)
	}
	ops := defaultStoreInitOps()
	ops.random = bytes.NewReader(append(first[:], second[:]...))
	result, err := prepareStoreDirectoryWithOps(dir, ops)
	if err != nil || result.status != "created" {
		t.Fatalf("collision result = %+v, err = %v", result, err)
	}
	contents, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil || string(contents) != "preserve" {
		t.Fatalf("collision occupant changed: %q, %v", contents, err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, name), []byte("preserve"), 0o644); err != nil {
		t.Fatal(err)
	}
	limited := defaultStoreInitOps()
	limited.random = bytes.NewReader(bytes.Repeat(first[:], 10))
	if _, err := prepareStoreDirectoryWithOps(other, limited); err == nil || !strings.Contains(err.Error(), "exhausted 10") {
		t.Fatalf("collision bound not enforced: %v", err)
	}
}

func TestStoreOnlyCompetingPublisherNeverReplacesMarker(t *testing.T) {
	for _, tc := range []struct {
		name      string
		compete   func(*testing.T, *os.Root)
		wantError bool
	}{
		{"regular marker", func(t *testing.T, root *os.Root) {
			file, err := root.OpenFile("MEMORY.md", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write([]byte("competitor")); err != nil {
				t.Fatal(err)
			}
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"directory marker", func(t *testing.T, root *os.Root) {
			if err := root.Mkdir("MEMORY.md", 0o755); err != nil {
				t.Fatal(err)
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ops := defaultStoreInitOps()
			ops.link = func(root *os.Root, oldname, newname string) error {
				tc.compete(t, root)
				return root.Link(oldname, newname)
			}
			result, err := prepareStoreDirectoryWithOps(dir, ops)
			if (err != nil) != tc.wantError {
				t.Fatalf("result = %+v, err = %v", result, err)
			}
			if !tc.wantError && (result.status != "already_prepared" || result.marker != "MEMORY.md") {
				t.Fatalf("result = %+v", result)
			}
			if !tc.wantError {
				content, err := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
				if err != nil || string(content) != "competitor" {
					t.Fatalf("competitor file replaced: %q, %v", content, err)
				}
			}
			assertNoStoreTemp(t, dir)
		})
	}
}

func TestStoreOnlyCompetingSymlinkNeverReplacesTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(target, []byte("external"), 0o644); err != nil {
		t.Fatal(err)
	}
	ops := defaultStoreInitOps()
	ops.link = func(root *os.Root, oldname, newname string) error {
		if err := os.Symlink(target, filepath.Join(dir, newname)); err != nil {
			return err
		}
		return root.Link(oldname, newname)
	}
	if _, err := prepareStoreDirectoryWithOps(dir, ops); err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("competing symlink accepted: %v", err)
	}
	info, err := os.Lstat(filepath.Join(dir, "MEMORY.md"))
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("competing symlink removed or replaced: %v, %v", info, err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "external" {
		t.Fatalf("external target changed: %q, %v", content, err)
	}
	assertNoStoreTemp(t, dir)
}

func TestStoreOnlyConcurrentRuntimeMayCreateDatabaseBeforeMarkerLink(t *testing.T) {
	dir := t.TempDir()
	ops := defaultStoreInitOps()
	ops.link = func(root *os.Root, oldname, newname string) error {
		file, err := root.OpenFile("gray.db", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		if _, err := file.Write([]byte("runtime data")); err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		return root.Link(oldname, newname)
	}
	result, err := prepareStoreDirectoryWithOps(dir, ops)
	if err != nil || result.status != "created" {
		t.Fatalf("concurrent database result = %+v, %v", result, err)
	}
	db, err := os.ReadFile(filepath.Join(dir, "gray.db"))
	if err != nil || string(db) != "runtime data" {
		t.Fatalf("database changed: %q, %v", db, err)
	}
	marker, err := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if err != nil || string(marker) != storeInitializationMarker {
		t.Fatalf("marker incomplete: %q, %v", marker, err)
	}
}

func TestStoreOnlyPublicationFailuresAndAmbiguity(t *testing.T) {
	t.Run("unsupported link", func(t *testing.T) {
		dir := t.TempDir()
		ops := defaultStoreInitOps()
		ops.link = func(*os.Root, string, string) error { return fs.ErrPermission }
		_, err := prepareStoreDirectoryWithOps(dir, ops)
		if err == nil || !strings.Contains(err.Error(), "without replacing existing files") {
			t.Fatalf("link failure = %v", err)
		}
		assertNoStoreTemp(t, dir)
		if _, err := os.Lstat(filepath.Join(dir, "MEMORY.md")); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("fallback published marker: %v", err)
		}
	})
	t.Run("link publishes then reports error", func(t *testing.T) {
		dir := t.TempDir()
		ops := defaultStoreInitOps()
		ops.link = func(root *os.Root, oldname, newname string) error {
			if err := root.Link(oldname, newname); err != nil {
				return err
			}
			return fs.ErrPermission
		}
		result, err := prepareStoreDirectoryWithOps(dir, ops)
		if err != nil || result.status != "already_prepared" {
			t.Fatalf("ambiguous link result = %+v, %v", result, err)
		}
		content, err := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
		if err != nil || string(content) != storeInitializationMarker {
			t.Fatalf("published marker incomplete: %q, %v", content, err)
		}
		assertNoStoreTemp(t, dir)
	})
	t.Run("unconfirmable link", func(t *testing.T) {
		dir := t.TempDir()
		ops := defaultStoreInitOps()
		lstat := ops.lstat
		published := false
		ops.link = func(root *os.Root, oldname, newname string) error {
			if err := root.Link(oldname, newname); err != nil {
				return err
			}
			published = true
			return fs.ErrPermission
		}
		ops.lstat = func(root *os.Root, name string) (fs.FileInfo, error) {
			if published {
				return nil, fs.ErrPermission
			}
			return lstat(root, name)
		}
		_, err := prepareStoreDirectoryWithOps(dir, ops)
		if err == nil || !strings.Contains(err.Error(), "could not be confirmed") {
			t.Fatalf("unconfirmed link = %v", err)
		}
		content, err := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
		if err != nil || string(content) != storeInitializationMarker {
			t.Fatalf("uncertain publication lost marker: %q, %v", content, err)
		}
		assertNoStoreTemp(t, dir)
	})
}

func TestStoreOnlyPrepublicationFailuresNeverPublish(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*storeInitOps, *int)
	}{
		{"open", func(ops *storeInitOps, closed *int) {
			ops.openFile = func(*os.Root, string, int, fs.FileMode) (*os.File, error) { return nil, fs.ErrPermission }
		}},
		{"short write", func(ops *storeInitOps, closed *int) {
			ops.writeFile = func(f *os.File, p []byte) (int, error) { return f.Write(p[:1]) }
		}},
		{"write error", func(ops *storeInitOps, closed *int) {
			ops.writeFile = func(f *os.File, p []byte) (int, error) {
				_, _ = f.Write(p[:1])
				return 1, fs.ErrPermission
			}
		}},
		{"sync", func(ops *storeInitOps, closed *int) {
			ops.syncFile = func(*os.File) error { return fs.ErrPermission }
		}},
		{"close", func(ops *storeInitOps, closed *int) {
			ops.closeFile = func(f *os.File) error { _ = f.Close(); return fs.ErrPermission }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ops := defaultStoreInitOps()
			closed := 0
			closeFile := ops.closeFile
			ops.closeFile = func(f *os.File) error { closed++; return closeFile(f) }
			tc.edit(&ops, &closed)
			_, err := prepareStoreDirectoryWithOps(dir, ops)
			if err == nil {
				t.Fatal("failure was ignored")
			}
			if tc.name != "open" && tc.name != "close" && closed != 1 {
				t.Fatalf("file Close calls = %d, want 1", closed)
			}
			if _, err := os.Lstat(filepath.Join(dir, "MEMORY.md")); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("incomplete marker published: %v", err)
			}
			assertNoStoreTemp(t, dir)
		})
	}
	t.Run("mkdir", func(t *testing.T) {
		data := filepath.Join(t.TempDir(), "absent", ".graymatter")
		ops := defaultStoreInitOps()
		ops.mkdirAll = func(*os.Root, string, fs.FileMode) error { return fs.ErrPermission }
		if _, err := prepareStoreDirectoryWithOps(data, ops); err == nil {
			t.Fatal("mkdir failure was ignored")
		}
		if _, err := os.Lstat(data); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("data directory created after injected failure: %v", err)
		}
	})
}

func TestStoreOnlyPostpublicationFailuresRetainMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*storeInitOps)
	}{
		{"cleanup", func(ops *storeInitOps) {
			ops.remove = func(*os.Root, string) error { return fs.ErrPermission }
		}},
		{"root close", func(ops *storeInitOps) {
			ops.closeRoot = func(root *os.Root) error { _ = root.Close(); return fs.ErrPermission }
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			ops := defaultStoreInitOps()
			tc.edit(&ops)
			_, err := prepareStoreDirectoryWithOps(dir, ops)
			if err == nil || !strings.Contains(err.Error(), "store prepared") || !strings.Contains(err.Error(), "marker retained") {
				t.Fatalf("postpublication error = %v", err)
			}
			content, err := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
			if err != nil || string(content) != storeInitializationMarker {
				t.Fatalf("marker lost after postpublication failure: %q, %v", content, err)
			}
			result, err := prepareStoreDirectory(dir)
			if err != nil || result.status != "already_prepared" {
				t.Fatalf("retry result = %+v, %v", result, err)
			}
		})
	}
}

func TestStoreOnlyDirectoryWithExistingMarkerNeedsNoWrite(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "MEMORY.md")
	if err := os.WriteFile(marker, []byte("existing"), 0o444); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(marker)
	if err != nil {
		t.Fatal(err)
	}
	ops := defaultStoreInitOps()
	ops.openFile = func(*os.Root, string, int, fs.FileMode) (*os.File, error) {
		t.Fatal("attempted to create a temporary in an already prepared store")
		return nil, nil
	}
	result, err := prepareStoreDirectoryWithOps(dir, ops)
	if err != nil || result.status != "already_prepared" {
		t.Fatalf("read-only store result = %+v, %v", result, err)
	}
	after, err := os.Stat(marker)
	if err != nil || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("existing marker changed: %v", err)
	}
}

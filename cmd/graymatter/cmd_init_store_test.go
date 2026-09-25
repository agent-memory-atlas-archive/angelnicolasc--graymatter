package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestStoreOnlyValidationBeforeFilesystem(t *testing.T) {
	base := t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
		edit func(*storeOnlyInitOptions)
	}{
		{"positional", []string{"extra"}, nil},
		{"global", nil, func(o *storeOnlyInitOptions) { o.global = true }},
		{"hooks", nil, func(o *storeOnlyInitOptions) { o.hooks = true }},
		{"interactive", nil, func(o *storeOnlyInitOptions) { o.interactive = true }},
		{"antigravity", nil, func(o *storeOnlyInitOptions) { o.withAntigravity = true }},
		{"kg", nil, func(o *storeOnlyInitOptions) { o.kg = true }},
		{"only even empty", nil, func(o *storeOnlyInitOptions) { o.onlyChanged = true }},
		{"empty path", nil, func(o *storeOnlyInitOptions) { o.dataDir = "" }},
		{"whitespace path", nil, func(o *storeOnlyInitOptions) { o.dataDir = "  \t  " }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := filepath.Join(base, tc.name, ".graymatter")
			opts := storeOnlyInitOptions{dataDir: data}
			if tc.edit != nil {
				tc.edit(&opts)
			}
			if _, err := validateStoreOnlyInit(tc.args, opts); err == nil {
				t.Fatal("invalid input accepted")
			}
			if _, err := os.Lstat(filepath.Join(base, tc.name)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("validation wrote to filesystem: %v", err)
			}
		})
	}
	valid := filepath.Join(base, " dir ü ", ".graymatter")
	got, err := validateStoreOnlyInit(nil, storeOnlyInitOptions{dataDir: valid})
	if err != nil || got != valid {
		t.Fatalf("valid path = %q, %v; want %q", got, err, valid)
	}
}

func TestStoreOnlyExplicitOnlyFlagAndPositionalAreRejected(t *testing.T) {
	oldDir, oldQuiet, oldJSON := dataDir, quiet, jsonOut
	t.Cleanup(func() { dataDir, quiet, jsonOut = oldDir, oldQuiet, oldJSON })
	for _, arg := range [][]string{
		{"--store-only", "--only="},
		{"--store-only", "--only=,,,"},
		{"--store-only", "--", "extra"},
	} {
		dataDir, quiet, jsonOut = filepath.Join(t.TempDir(), ".graymatter"), true, true
		cmd := initCmd()
		cmd.SetArgs(arg)
		cmd.SilenceUsage = true
		cmd.SilenceErrors = true
		var out bytes.Buffer
		cmd.SetOut(&out)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("args %q accepted", arg)
		}
		if out.Len() != 0 {
			t.Fatalf("args %q emitted success output: %q", arg, out.String())
		}
		if _, err := os.Lstat(dataDir); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("args %q created data dir: %v", arg, err)
		}
	}
}

func TestStoreOnlyExplicitFalseKeepsLegacyInit(t *testing.T) {
	oldDir, oldQuiet, oldJSON := dataDir, quiet, jsonOut
	t.Cleanup(func() { dataDir, quiet, jsonOut = oldDir, oldQuiet, oldJSON })
	project := t.TempDir()
	oldCwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCwd) })
	for _, env := range []string{"HOME", "USERPROFILE", "APPDATA", "LOCALAPPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(env, t.TempDir())
	}
	dataDir, quiet, jsonOut = filepath.Join(project, ".graymatter"), true, false
	cmd := initCmd()
	cmd.SetArgs([]string{"--store-only=false", "--only=claudecode", "--skip-instructions", "--no-path"})
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(project, ".mcp.json")); err != nil {
		t.Fatalf("legacy MCP writer was bypassed: %v", err)
	}
}

func TestStoreOnlyOutputContract(t *testing.T) {
	path := filepath.Join(t.TempDir(), "data")
	for _, tc := range []struct {
		name string
		opts storeOnlyInitOptions
		want string
	}{
		{"human created", storeOnlyInitOptions{}, "GrayMatter store prepared at " + path + " (created MEMORY.md).\n"},
		{"quiet", storeOnlyInitOptions{quiet: true}, ""},
		{"JSON wins over quiet", storeOnlyInitOptions{quiet: true, json: true}, "json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			cmd := &cobra.Command{}
			cmd.SetOut(&out)
			result := storeOnlyInitResult{1, "store-only", path, "created", "MEMORY.md", false}
			if err := renderStoreOnlyInitResult(cmd, result, tc.opts); err != nil {
				t.Fatal(err)
			}
			if tc.want == "json" {
				var got map[string]any
				if err := json.Unmarshal(out.Bytes(), &got); err != nil {
					t.Fatal(err)
				}
				want := map[string]any{
					"schema_version": float64(1), "mode": "store-only", "data_dir": path,
					"status": "created", "marker": "MEMORY.md", "runtime_verified": false,
				}
				if !reflect.DeepEqual(got, want) || !bytes.HasSuffix(out.Bytes(), []byte("\n")) {
					t.Fatalf("JSON output = %q", out.String())
				}
			} else if out.String() != tc.want {
				t.Fatalf("output = %q, want %q", out.String(), tc.want)
			}
		})
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	result := storeOnlyInitResult{1, "store-only", path, "already_prepared", "gray.db", false}
	if err := renderStoreOnlyInitResult(cmd, result, storeOnlyInitOptions{}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "GrayMatter store already prepared at "+path+" (gray.db).\n" {
		t.Fatalf("existing output = %q", out.String())
	}
}

type storeOnlyFailingWriter struct {
	partial bool
}

func (w storeOnlyFailingWriter) Write(p []byte) (int, error) {
	if w.partial && len(p) > 0 {
		return 1, io.ErrClosedPipe
	}
	return 0, io.ErrClosedPipe
}

func TestStoreOnlyOutputFailureKeepsPreparedMarker(t *testing.T) {
	for _, jsonOutput := range []bool{false, true} {
		for _, partial := range []bool{false, true} {
			dir := filepath.Join(t.TempDir(), ".graymatter")
			cmd := &cobra.Command{}
			cmd.SetOut(storeOnlyFailingWriter{partial: partial})
			opts := storeOnlyInitOptions{dataDir: dir, json: jsonOutput}
			if err := runStoreOnlyInit(cmd, nil, opts); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("json=%v partial=%v: error = %v", jsonOutput, partial, err)
			}
			content, err := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
			if err != nil || string(content) != storeInitializationMarker {
				t.Fatalf("marker after output failure = %q, %v", content, err)
			}
			result, err := prepareStoreDirectory(dir)
			if err != nil || result.status != "already_prepared" {
				t.Fatalf("retry = %+v, %v", result, err)
			}
		}
	}
}

func TestStoreOnlyPreservesClientFilesAndBypassesPath(t *testing.T) {
	project := t.TempDir()
	data := filepath.Join(project, ".graymatter")
	files := map[string]string{
		".mcp.json": "{broken JSON",
		"CLAUDE.md": "custom instructions",
		"AGENTS.md": "custom instructions",
		filepath.Join(".claude", "settings.json"): "{broken JSON",
	}
	for name, content := range files {
		path := filepath.Join(project, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	type snapshot struct {
		content []byte
		mtime   int64
	}
	before := make(map[string]snapshot)
	for name := range files {
		path := filepath.Join(project, name)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		before[name] = snapshot{content, info.ModTime().UnixNano()}
	}
	oldSeam := initAddExeDirToUserPath
	called := 0
	initAddExeDirToUserPath = func() (bool, error) { called++; return false, nil }
	t.Cleanup(func() { initAddExeDirToUserPath = oldSeam })
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	opts := storeOnlyInitOptions{dataDir: data, quiet: true}
	if err := runStoreOnlyInit(cmd, nil, opts); err != nil {
		t.Fatal(err)
	}
	if called != 0 || out.Len() != 0 {
		t.Fatalf("PATH callback called %d times; stdout=%q", called, out.String())
	}
	for name := range files {
		path := filepath.Join(project, name)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(content, before[name].content) || info.ModTime().UnixNano() != before[name].mtime {
			t.Fatalf("client file %s changed", name)
		}
	}
	if entries, err := os.ReadDir(data); err != nil || len(entries) != 1 || !strings.EqualFold(entries[0].Name(), "MEMORY.md") {
		t.Fatalf("data dir entries = %v, %v", entries, err)
	}
}

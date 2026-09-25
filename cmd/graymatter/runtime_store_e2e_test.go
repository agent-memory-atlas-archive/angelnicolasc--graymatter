package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPNoCreateRejectsBeforeEffectsAndAcceptsPreparedStore(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "new-project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))

	for _, args := range [][]string{
		{"--no-daemon", "mcp", "serve", "--no-create"},
		{"mcp", "serve", "--no-create"},
		{"mcp", "serve", "--no-create", "--http", "127.0.0.1:0"},
		{"--quiet", "mcp", "serve", "--no-create"},
	} {
		out := f.run(project, "", args...)
		if out.code != 1 || out.stdout != "" || !strings.Contains(out.stderr, "store is not prepared") {
			t.Fatalf("unprepared MCP %q: %+v", args, out)
		}
		if _, err := os.Lstat(store); !os.IsNotExist(err) {
			t.Fatalf("rejected MCP changed absent store: %v", err)
		}
	}

	for _, args := range [][]string{
		{"mcp", "serve", "--http", "not-a-listen-address"},
		{"mcp", "serve", "--http", ":0", "--no-auth"},
	} {
		out := f.run(project, "", args...)
		if out.code != 1 || out.stdout != "" {
			t.Fatalf("invalid MCP options %q: %+v", args, out)
		}
		if _, err := os.Lstat(store); !os.IsNotExist(err) {
			t.Fatalf("invalid MCP options changed absent store: %v", err)
		}
	}

	f.mustRun(project, "", "init", "--store-only")
	issue81AssertOnlyMarker(t, store)
	m := f.startMCP(project, f.bin, []string{"--no-daemon", "mcp", "serve", "--no-create"}, nil)
	m.add(3, "new-project", "prepared store can serve MCP")
	m.search(4, "new-project", "prepared store", "prepared store can serve MCP")
	m.close()
	if _, err := os.Stat(filepath.Join(store, "gray.db")); err != nil {
		t.Fatalf("accepted MCP did not open prepared store: %v", err)
	}

	eagerProject := filepath.Join(f.root, "eager-project")
	if err := os.Mkdir(eagerProject, 0o755); err != nil {
		t.Fatal(err)
	}
	eagerStore := f.registerStore(filepath.Join(eagerProject, ".graymatter"))
	eagerMCP := f.startMCP(eagerProject, f.bin,
		[]string{"--no-daemon", "mcp", "serve", "--no-create=false"}, nil)
	eagerMCP.close()
	if _, err := os.Stat(filepath.Join(eagerStore, "gray.db")); err != nil {
		t.Fatalf("eager mode did not create database: %v", err)
	}
}

func TestMCPEagerRejectsNonRegularDatabaseBeforeOpeningIt(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "unsafe-db-project")
	store := filepath.Join(project, ".graymatter")
	if err := os.MkdirAll(filepath.Join(store, "gray.db"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "MEMORY.md"), []byte("marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--no-daemon", "mcp", "serve"},
		{"mcp", "serve"},
		{"--no-daemon", "mcp", "serve", "--no-create"},
		{"mcp", "serve", "--no-create"},
	} {
		out := f.run(project, "", args...)
		if out.code != 1 || out.stdout != "" || !strings.Contains(out.stderr, "gray.db is not a regular file") {
			t.Fatalf("unsafe DB MCP %q: %+v", args, out)
		}
	}
	packet, err := json.Marshal(map[string]string{"cwd": project, "prompt": "remember: must not write"})
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"--no-daemon", "hooks", "run", "user-prompt"},
		{"--no-daemon", "hooks", "run", "user-prompt", "--no-create"},
	} {
		out := f.run(project, string(packet), args...)
		if out.code != 0 || out.stdout != "" || !strings.Contains(out.stderr, "invalid store entry") {
			t.Fatalf("unsafe DB hook %q: %+v", args, out)
		}
	}
	if _, err := os.Stat(filepath.Join(store, "hooks.log")); !os.IsNotExist(err) {
		t.Fatalf("rejected MCP wrote hooks log: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store, "graymatter.http-token")); !os.IsNotExist(err) {
		t.Fatalf("rejected MCP minted HTTP token: %v", err)
	}
}

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
)

func TestRuntimeHTTPNoAuthLoopbackUsesSelectedStore(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "http-no-auth-project")
	if err := os.Mkdir(project, 0o755); err != nil {
		t.Fatal(err)
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	f.mustRun(project, "", "init", "--store-only")
	addr := runtimeFreeTCPAddress(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	cmd := exec.CommandContext(ctx, f.bin, "--no-daemon", "mcp", "serve", "--http", addr, "--no-auth")
	cmd.Dir, cmd.Env = project, issue81Env(f.env, nil)
	var stdout, stderr issue81LockedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() { cancel(); _ = cmd.Wait() }()

	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
			"clientInfo": map[string]string{"name": "routing-no-auth-test", "version": "1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", bytes.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr != nil || resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("serverInfo")) {
				t.Fatalf("loopback --no-auth initialize: status=%d body=%q err=%v", resp.StatusCode, body, readErr)
			}
			if _, err := os.Stat(filepath.Join(store, "gray.db")); err != nil {
				t.Fatalf("accepted HTTP MCP did not open selected store: %v", err)
			}
			if _, err := os.Lstat(filepath.Join(store, "graymatter.http-token")); !os.IsNotExist(err) {
				t.Fatalf("--no-auth unexpectedly created token: %v", err)
			}
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("loopback --no-auth MCP did not listen: stdout=%q stderr=%q", stdout.String(), stderr.String())
}

func runtimeFreeTCPAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return addr
}

func TestRuntimeDaemonRouteSurvivesRestartAndKeepsSharedNamespace(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "daemon-project")
	subdir := filepath.Join(project, "src")
	process := filepath.Join(f.root, "other-process")
	for _, dir := range []string{subdir, process} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	otherStore := filepath.Join(process, ".graymatter")
	f.mustRun(project, "", "init", "--store-only")
	env := map[string]string{"CLAUDE_PROJECT_DIR": project}
	m := f.startMCP(process, f.bin, []string{"mcp", "serve"}, env)
	m.add(3, "daemon-project", "daemon route survives first connection")
	packet := issue81HookPayload(t, subdir, "remember: hook and MCP use one daemon store")
	out := f.runCommand(f.bin, process, packet, env, "hooks", "run", "user-prompt")
	if out.code != 0 || !strings.Contains(out.stdout, "Saved to memory (daemon-project)") {
		t.Fatalf("daemon-backed hook route failed: %+v", out)
	}
	m.search(4, "daemon-project", "one daemon store", "hook and MCP use one daemon store")
	shared := issue81HookPayload(t, subdir, "remember shared: shared daemon route remains selected")
	out = f.runCommand(f.bin, process, shared, env, "hooks", "run", "user-prompt")
	if out.code != 0 || !strings.Contains(out.stdout, "Saved to shared memory") {
		t.Fatalf("daemon-backed shared hook route failed: %+v", out)
	}
	m.search(5, "__shared__", "shared daemon route", "shared daemon route remains selected")
	if out := f.run(process, "", "--dir", store, "daemon", "stop"); out.code != 0 {
		t.Fatalf("stop fixture daemon: %+v", out)
	}
	deadline := time.Now().Add(8 * time.Second)
	stopped := false
	for time.Now().Before(deadline) {
		status := f.run(process, "", "--dir", store, "daemon", "status")
		if status.code == 0 && strings.Contains(status.stdout, "not running") {
			stopped = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !stopped {
		t.Fatal("fixture daemon did not stop before reconnect check")
	}
	m.add(6, "daemon-project", "reconnected to the original selected store")
	m.search(7, "daemon-project", "reconnected original selected", "reconnected to the original selected store")
	m.close()
	if _, err := os.Lstat(otherStore); !os.IsNotExist(err) {
		t.Fatalf("daemon route created process-cwd store: %v", err)
	}
}

func TestRuntimeSessionEndDetachedChildUsesExplicitStore(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "detached-project")
	subdir := filepath.Join(project, "src")
	process := filepath.Join(f.root, "detached-process")
	for _, dir := range []string{subdir, process} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := f.registerStore(filepath.Join(f.root, "explicit-detached-store"))
	agent := "detached-project"
	const decayedFact = "ancient route-specific fact awaiting detached prune"
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = store
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.VectorReconcileInterval = 0
	cfg.AsyncConsolidate = false
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Remember(context.Background(), agent, decayedFact); err != nil {
		_ = mem.Close()
		t.Fatal(err)
	}
	advanced := mem.Advanced()
	facts, err := advanced.List(agent)
	if err != nil {
		_ = mem.Close()
		t.Fatal(err)
	}
	var found bool
	for _, fact := range facts {
		if fact.Text == decayedFact {
			fact.Weight = 0.005
			if err := advanced.UpdateFact(agent, fact); err != nil {
				_ = mem.Close()
				t.Fatal(err)
			}
			found = true
		}
	}
	if err := mem.Close(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("decay fixture fact was not stored")
	}
	before := f.run(process, "", "--dir", store, "recall", agent, "ancient route-specific", "--top-k", "8", "--quiet")
	if before.code != 0 || !strings.Contains(before.stdout, decayedFact) {
		t.Fatalf("prune fixture fact not visible before SessionEnd: %+v", before)
	}
	payload, err := json.Marshal(map[string]string{"cwd": subdir, "session_id": "detached-routing-session"})
	if err != nil {
		t.Fatal(err)
	}
	out := f.runCommand(f.bin, process, string(payload), map[string]string{"CLAUDE_PROJECT_DIR": project},
		"--dir", store, "hooks", "run", "session-end")
	if out.code != 0 || out.stdout != "" {
		t.Fatalf("explicit SessionEnd hook failed: %+v", out)
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		recall := f.run(process, "", "--dir", store, "recall", agent, "ancient route-specific", "--top-k", "8", "--quiet")
		if recall.code == 0 && !strings.Contains(recall.stdout, decayedFact) {
			for _, wrong := range []string{filepath.Join(project, ".graymatter"), filepath.Join(process, ".graymatter")} {
				if _, err := os.Lstat(wrong); !os.IsNotExist(err) {
					t.Fatalf("detached route created wrong store %s: %v", wrong, err)
				}
			}
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	logData, _ := os.ReadFile(filepath.Join(store, "hooks.log"))
	t.Fatalf("detached consolidate did not prune fact in selected store; hooks.log=%q", logData)
}

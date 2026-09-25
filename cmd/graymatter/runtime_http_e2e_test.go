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
)

func TestMCPHTTPUsesServiceStoreAndKeepsAuthWithClaudeEnv(t *testing.T) {
	f := newIssue81Fixture(t)
	service := filepath.Join(f.root, "service-root")
	otherProject := filepath.Join(f.root, "claude-project")
	for _, dir := range []string{service, otherProject} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := f.registerStore(filepath.Join(service, ".graymatter"))
	otherStore := f.registerStore(filepath.Join(otherProject, ".graymatter"))
	f.mustRun(service, "", "init", "--store-only")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	cmd := exec.CommandContext(ctx, f.bin, "--no-daemon", "mcp", "serve", "--http", addr)
	cmd.Dir = service
	cmd.Env = issue81Env(f.env, map[string]string{"CLAUDE_PROJECT_DIR": otherProject})
	var stdout, stderr issue81LockedBuffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	defer func() {
		cancel()
		_ = cmd.Wait()
	}()

	initialize, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26", "capabilities": map[string]any{},
			"clientInfo": map[string]string{"name": "routing-test", "version": "1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Timeout: time.Second}
	post := func(token string) (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", bytes.NewReader(initialize))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		return client.Do(req)
	}
	deadline := time.Now().Add(8 * time.Second)
	var response *http.Response
	for time.Now().Before(deadline) {
		response, err = post("")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("MCP HTTP did not listen: %v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("HTTP auth default changed: status=%d", response.StatusCode)
	}
	tokenBytes, err := os.ReadFile(filepath.Join(store, "graymatter.http-token"))
	if err != nil {
		t.Fatalf("service store token missing: %v", err)
	}
	response, err = post(strings.TrimSpace(string(tokenBytes)))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if readErr != nil || response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("serverInfo")) {
		t.Fatalf("authenticated initialize: status=%d body=%q err=%v", response.StatusCode, body, readErr)
	}
	if _, err := os.Stat(filepath.Join(store, "gray.db")); err != nil {
		t.Fatalf("HTTP did not open service store: %v", err)
	}
	if _, err := os.Lstat(otherStore); !os.IsNotExist(err) {
		t.Fatalf("HTTP inherited Claude root as remote store: %v", err)
	}
}

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
)

// Both transports use one store throughout: hook, MCP search and correction,
// another hook, and a fresh owner after closing the original owner.
func TestLexicalHookStore_PublicLifecycle(t *testing.T) {
	bin := buildE2EBinary(t)
	for _, key := range []string{"OPENAI_API_KEY", "VOYAGE_API_KEY", "ANTHROPIC_API_KEY", "GRAYMATTER_CONSOLIDATE_LLM", "GRAYMATTER_KG", "GRAYMATTER_NO_DAEMON"} {
		t.Setenv(key, "")
	}
	t.Setenv("GRAYMATTER_OLLAMA_URL", "http://127.0.0.1:1")
	for _, direct := range []bool{true, false} {
		name := "daemon"
		if direct {
			name = "direct"
		}
		t.Run(name, func(t *testing.T) {
			project := filepath.Join(t.TempDir(), "parcel-project")
			if err := os.MkdirAll(project, 0o700); err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(project, ".graymatter")
			cfg := lexicalStoreConfig(dir)
			mem, err := graymatter.NewWithConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			const old = "Parcel dispatch cutoff is 16:00 for project shipments."
			const replacement = "Parcel dispatch cutoff is 18:00 for project shipments."
			const shared = "Parcel dispatch requires a signed manifest."
			for agent, text := range map[string]string{"parcel-project": old, "__shared__": shared, "other-project": "Parcel dispatch cutoff is midnight for the foreign project."} {
				if err := mem.Remember(context.Background(), agent, text); err != nil {
					_ = mem.Close()
					t.Fatal(err)
				}
			}
			if err := mem.Close(); err != nil {
				t.Fatal(err)
			}
			if !direct {
				t.Cleanup(func() { stopE2EDaemon(t, bin, project, dir) })
			}
			run := func(input string, args ...string) string {
				t.Helper()
				prefix := []string{"--dir", dir}
				if direct {
					prefix = append(prefix, "--no-daemon")
				}
				out, code := runE2E(t, bin, project, input, append(prefix, args...)...)
				if code != 0 {
					t.Fatalf("%v exit=%d: %s", args, code, out)
				}
				return out
			}
			hook := func(session string) string {
				payload, err := json.Marshal(map[string]string{"session_id": session, "cwd": project, "hook_event_name": "UserPromptSubmit", "prompt": "What is the parcel dispatch cutoff and manifest requirement?"})
				if err != nil {
					t.Fatal(err)
				}
				return run(string(payload), "hooks", "run", "user-prompt", "--packet-policy", "lexical")
			}
			first := hook("before-correction")
			if !strings.Contains(first, old) || !strings.Contains(first, shared) || strings.Contains(first, "midnight") {
				t.Fatalf("initial packet: %q", first)
			}
			call := func(tool string, arguments map[string]any) map[string]any {
				t.Helper()
				request, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": tool, "arguments": arguments}})
				if err != nil {
					t.Fatal(err)
				}
				input := "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2024-11-05\",\"capabilities\":{},\"clientInfo\":{\"name\":\"hook-test\",\"version\":\"1\"}}}\n{\"jsonrpc\":\"2.0\",\"method\":\"notifications/initialized\"}\n" + string(request) + "\n"
				output := run(input, "mcp", "serve")
				for _, line := range strings.Split(output, "\n") {
					var response map[string]any
					if json.Unmarshal([]byte(line), &response) != nil || response["id"] != float64(2) {
						continue
					}
					result, ok := response["result"].(map[string]any)
					if !ok || result["isError"] == true || response["error"] != nil {
						t.Fatalf("MCP %s failed: %s", tool, line)
					}
					return result
				}
				t.Fatalf("missing MCP response: %s", output)
				return nil
			}
			search := func() string {
				x := call("memory_search", map[string]any{"agent_id": "parcel-project", "query": "parcel dispatch cutoff", "top_k": 8})
				data, err := json.Marshal(x)
				if err != nil {
					t.Fatal(err)
				}
				return string(data)
			}
			if out := search(); !strings.Contains(out, old) || strings.Contains(out, "midnight") {
				t.Fatalf("MCP search after hook: %s", out)
			}
			call("memory_reflect", map[string]any{"agent_id": "parcel-project", "action": "update", "target": old, "text": replacement})
			updated := hook("after-correction")
			if !strings.Contains(updated, replacement) || !strings.Contains(updated, shared) || strings.Contains(updated, old) || strings.Contains(updated, "midnight") {
				t.Fatalf("corrected packet: %q", updated)
			}
			if out := search(); !strings.Contains(out, replacement) || strings.Contains(out, old) {
				t.Fatalf("MCP search after correction: %s", out)
			}
			if !direct {
				stopE2EDaemon(t, bin, project, dir)
			}
			reopened, err := graymatter.NewWithConfig(cfg)
			if err != nil {
				t.Fatal(err)
			}
			facts, err := reopened.Advanced().List("parcel-project")
			if err != nil {
				_ = reopened.Close()
				t.Fatal(err)
			}
			live, retired := 0, 0
			for _, fact := range facts {
				if fact.Text == old && fact.IsSuperseded() {
					retired++
				}
				if fact.Text == replacement && !fact.IsSuperseded() {
					live++
				}
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			if live != 1 || retired != 1 {
				t.Fatalf("reopened revision: live=%d retired=%d", live, retired)
			}
			if out := hook("reopened-owner"); !strings.Contains(out, replacement) || strings.Contains(out, old) {
				t.Fatalf("reopened hook: %s", out)
			}
		})
	}
}

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
)

func adjacentSetupJSON(t *testing.T, out issue81Output, exit int, status, phase string) map[string]any {
	t.Helper()
	if out.code != exit {
		t.Fatalf("setup exit=%d want=%d stdout=%q stderr=%q err=%v", out.code, exit, out.stdout, out.stderr, out.err)
	}
	decoder := json.NewDecoder(strings.NewReader(out.stdout))
	var report map[string]any
	if err := decoder.Decode(&report); err != nil {
		t.Fatalf("setup did not emit JSON: %v stdout=%q", err, out.stdout)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		t.Fatalf("setup emitted more than one JSON document: %v stdout=%q", err, out.stdout)
	}
	if report["schema_version"] != float64(1) || report["mode"] != "setup" || report["status"] != status || report["phase"] != phase {
		t.Fatalf("setup report: %#v", report)
	}
	if _, ok := report["actions"].([]any); !ok {
		t.Fatalf("setup actions are absent or null: %#v", report["actions"])
	}
	if _, ok := report["errors"].([]any); !ok {
		t.Fatalf("setup errors are absent or null: %#v", report["errors"])
	}
	return report
}

func adjacentManagedHookSpec(t *testing.T, settingsPath, event string) (string, []string) {
	t.Helper()
	for _, group := range hookGroups(t, readSettings(t, settingsPath), event) {
		entries, _ := group["hooks"].([]any)
		for _, raw := range entries {
			hook, _ := raw.(map[string]any)
			if hook == nil || !hookEntryHasArg(hook, hooksCommandMarker) {
				continue
			}
			command, _ := hook["command"].(string)
			args, ok := hookEntryArgs(hook)
			if command == "" || !ok {
				t.Fatalf("managed hook is not command/args form: %v", hook)
			}
			return command, args
		}
	}
	t.Fatalf("no managed %s hook in %s", event, settingsPath)
	return "", nil
}

func adjacentHasDirArg(args []string, store string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--dir" && args[i+1] == store {
			return true
		}
	}
	return false
}

type adjacentClientCommand struct {
	name    string
	command string
	args    []string
}

func adjacentInstalledMCPCommands(t *testing.T, f *issue81Fixture, project string, wantArgs []string) []adjacentClientCommand {
	t.Helper()
	claude := issue81MCPFromConfig(t, filepath.Join(project, ".mcp.json"))
	if claude.Command != "graymatter" || !reflect.DeepEqual(claude.Args, wantArgs) {
		t.Fatalf("Claude command/args: %+v want=%v", claude, wantArgs)
	}
	var codex struct {
		Servers map[string]struct {
			Command string   `toml:"command"`
			Args    []string `toml:"args"`
		} `toml:"mcp_servers"`
	}
	if _, err := toml.DecodeFile(filepath.Join(f.home, ".codex", "config.toml"), &codex); err != nil {
		t.Fatal(err)
	}
	if server := codex.Servers["graymatter"]; server.Command != "graymatter" || !reflect.DeepEqual(server.Args, wantArgs) {
		t.Fatalf("Codex command/args: %+v want=%v", server, wantArgs)
	}
	openBytes, err := os.ReadFile(filepath.Join(project, "opencode.jsonc"))
	if err != nil {
		t.Fatal(err)
	}
	var opencode struct {
		MCP map[string]struct {
			Command []string `json:"command"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal(openBytes, &opencode); err != nil {
		t.Fatal(err)
	}
	wantOpenCode := append([]string{"graymatter"}, wantArgs...)
	openCodeArgs := opencode.MCP["graymatter"].Command
	if !reflect.DeepEqual(openCodeArgs, wantOpenCode) {
		t.Fatalf("OpenCode command=%v want=%v", openCodeArgs, wantOpenCode)
	}
	return []adjacentClientCommand{
		{"Claude", claude.Command, claude.Args},
		{"Codex", codex.Servers["graymatter"].Command, codex.Servers["graymatter"].Args},
		{"OpenCode", openCodeArgs[0], openCodeArgs[1:]},
	}
}

func adjacentStructuredTool(t *testing.T, m *issue81MCP, id int, name string, arguments map[string]any) map[string]any {
	t.Helper()
	result := m.request(id, "tools/call", map[string]any{"name": name, "arguments": arguments})
	var envelope struct {
		StructuredContent map[string]any `json:"structuredContent"`
	}
	if err := json.Unmarshal(result, &envelope); err != nil || envelope.StructuredContent == nil {
		t.Fatalf("%s returned no structured result: %s err=%v", name, result, err)
	}
	return envelope.StructuredContent
}

// R09: a rejected cold connection is followed by store-only preparation, then
// a real stdio session exercises all seven tools rather than only listing them.
func TestAdjacentR09AllSevenToolsAfterPreparation(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "seven-tools-project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	cold := f.run(project, "", "--no-daemon", "mcp", "serve", "--no-create")
	if cold.code != 1 || cold.stdout != "" || !strings.Contains(cold.stderr, "store is not prepared") {
		t.Fatalf("cold guarded MCP: %+v", cold)
	}
	if _, err := os.Lstat(store); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cold MCP created store: %v", err)
	}
	issue81Result(t, f.run(project, "", "init", "--store-only", "--json"), "created", "MEMORY.md", store)
	m := f.startMCP(project, f.bin, []string{"--no-daemon", "mcp", "serve", "--no-create"}, nil)
	const agent = "seven-tools-project"
	const aliasFact = "graniteword identifies the complete tool runtime"
	const reflectFact = "reflection handler persists after guarded reconnection"
	m.add(3, agent, aliasFact)
	m.search(4, agent, "graniteword", aliasFact)
	alias := adjacentStructuredTool(t, m, 5, "memory_alias", map[string]any{
		"agent_id": agent, "term": "bridgeword", "equivalents": []string{"graniteword"},
	})
	if alias["stored"] != true || alias["term"] != "bridgeword" || alias["agent_id"] != agent {
		t.Fatalf("memory_alias did not store mapping: %#v", alias)
	}
	m.search(6, agent, "bridgeword", aliasFact)
	reflected := adjacentStructuredTool(t, m, 7, "memory_reflect", map[string]any{
		"agent_id": agent, "action": "add", "text": reflectFact,
	})
	if reflected["ok"] != true || reflected["action"] != "add" || reflected["agent"] != agent {
		t.Fatalf("memory_reflect did not add fact: %#v", reflected)
	}
	m.search(8, agent, "reflection handler", reflectFact)
	saved := adjacentStructuredTool(t, m, 9, "checkpoint_save", map[string]any{
		"agent_id": agent, "state": `{"phase":"reconnected","selected_store":"seven-tools"}`,
	})
	checkpointID, _ := saved["checkpoint_id"].(string)
	if checkpointID == "" || saved["agent_id"] != agent {
		t.Fatalf("checkpoint_save did not persist state: %#v", saved)
	}
	resumed := adjacentStructuredTool(t, m, 10, "checkpoint_resume", map[string]any{"agent_id": agent})
	state, _ := resumed["state"].(map[string]any)
	if resumed["id"] != checkpointID || state["phase"] != "reconnected" || state["selected_store"] != "seven-tools" {
		t.Fatalf("checkpoint_resume did not restore latest state: %#v", resumed)
	}
	batch := adjacentStructuredTool(t, m, 11, "memory_search_batch", map[string]any{
		"agent_id": agent, "queries": []string{"graniteword", "reflection handler"}, "top_k": 8,
	})
	merged, _ := batch["merged"].([]any)
	want := map[string]bool{aliasFact: false, reflectFact: false}
	for _, raw := range merged {
		if fact, ok := raw.(string); ok {
			if _, exists := want[fact]; exists {
				want[fact] = true
			}
		}
	}
	for fact, found := range want {
		if !found {
			t.Fatalf("memory_search_batch omitted %q: %#v", fact, batch)
		}
	}
	m.close()
}

// E01: a custom global and project MCP definition survives normal init. The
// installed global hook and the MCP command then use the selected custom
// store, including its shared namespace.
func TestAdjacentE01CustomGlobalSetupAndRuntime(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	projectStore := f.registerStore(filepath.Join(project, ".graymatter"))
	customStore := f.registerStore(filepath.Join(f.root, "custom-store"))
	definition := map[string]any{
		"command": f.bin,
		"args":    []string{"--dir", customStore, "mcp", "serve", "--no-create"},
		"env":     map[string]string{"GRAYMATTER_OLLAMA_URL": "disabled://issue81-tests"},
	}
	globalConfig := filepath.Join(f.home, ".claude.json")
	projectConfig := filepath.Join(project, ".mcp.json")
	issue81WriteJSON(t, globalConfig, map[string]any{"mcpServers": map[string]any{"graymatter": definition, "other": map[string]string{"command": "foreign"}}, "other": "global setting"})
	issue81WriteJSON(t, projectConfig, map[string]any{"mcpServers": map[string]any{"graymatter": definition}, "other": "project setting"})
	f.mustRun(project, "", "--dir", customStore, "hooks", "install", "--scope", "global")
	settings := filepath.Join(f.home, ".claude", "settings.json")
	hookCommand, hookArgs := issue81HookSpec(t, settings, hooksEventUserPrompt)
	watches := map[string]issue81Snapshot{}
	for _, path := range []string{globalConfig, projectConfig, settings} {
		watches[path] = issue81SnapshotFile(t, path)
	}

	setup := f.run(project, "", "init", "--no-path", "--json")
	adjacentSetupJSON(t, setup, 0, "complete", "apply")
	for path, want := range watches {
		issue81AssertFileUnchanged(t, path, want)
	}
	if _, err := os.Stat(filepath.Join(projectStore, "MEMORY.md")); err != nil {
		t.Fatalf("normal init did not prepare its selected project store: %v", err)
	}
	issue81Result(t, f.run(project, "", "--dir", customStore, "init", "--store-only", "--json"), "created", "MEMORY.md", customStore)
	issue81AssertOnlyMarker(t, customStore)
	mcp := issue81MCPFromConfig(t, globalConfig)
	m := f.startMCP(project, mcp.Command, mcp.Args, mcp.Env)
	const projectFact = "custom global and project MCP retain this fact"
	m.add(3, "project", projectFact)
	m.search(4, "project", "custom global", projectFact)
	hook := f.runCommand(hookCommand, project, issue81HookPayload(t, project, "custom global retain"), nil, hookArgs...)
	if hook.code != 0 || !strings.Contains(hook.stdout, projectFact) {
		t.Fatalf("installed hook did not read custom store: %+v", hook)
	}
	shared := f.runCommand(hookCommand, project, issue81HookPayload(t, project, "remember shared: custom store shared fact"), nil, hookArgs...)
	if shared.code != 0 || !strings.Contains(shared.stdout, "Saved to shared memory") {
		t.Fatalf("installed hook did not write shared namespace: %+v", shared)
	}
	m.search(5, "__shared__", "custom store shared", "custom store shared fact")
	m.close()
	for path, want := range watches {
		issue81AssertFileUnchanged(t, path, want)
	}
}

// E02: one malformed client blocks the entire preflight. Removing that
// document lets setup create both selected clients, then retry idempotently.
func TestAdjacentE02PreflightAbortRepairAndRetry(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	claude := filepath.Join(project, ".mcp.json")
	cursor := filepath.Join(project, ".cursor", "mcp.json")
	if err := os.MkdirAll(filepath.Dir(cursor), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cursor, []byte(`{"mcpServers": {"graymatter":`), 0o600); err != nil {
		t.Fatal(err)
	}
	cursorBefore := issue81SnapshotFile(t, cursor)
	first := f.run(project, "", "init", "--no-path", "--only", "claudecode,cursor", "--json")
	report := adjacentSetupJSON(t, first, 1, "failed", "preflight")
	if report["changed"] != false || report["effects_uncertain"] != false {
		t.Fatalf("preflight claimed effects: %#v", report)
	}
	issue81AssertFileUnchanged(t, cursor, cursorBefore)
	if _, err := os.Lstat(claude); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight created Claude config: %v", err)
	}
	if _, err := os.Lstat(store); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("preflight created store: %v", err)
	}
	for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
		if _, err := os.Lstat(filepath.Join(project, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preflight created %s: %v", name, err)
		}
	}

	if err := os.Remove(cursor); err != nil {
		t.Fatalf("remove malformed Cursor config before retry: %v", err)
	}
	second := f.run(project, "", "init", "--no-path", "--only", "claudecode,cursor", "--json")
	adjacentSetupJSON(t, second, 0, "complete", "apply")
	if _, err := os.Stat(filepath.Join(store, "MEMORY.md")); err != nil {
		t.Fatalf("corrected setup did not prepare store: %v", err)
	}
	_ = issue81MCPFromConfig(t, claude)
	_ = issue81MCPFromConfig(t, cursor)
	claudeAfter := issue81SnapshotFile(t, claude)
	cursorAfter := issue81SnapshotFile(t, cursor)
	markerAfter := issue81SnapshotFile(t, filepath.Join(store, "MEMORY.md"))
	third := f.run(project, "", "init", "--no-path", "--only", "claudecode,cursor", "--json")
	final := adjacentSetupJSON(t, third, 0, "complete", "apply")
	if final["changed"] != false {
		t.Fatalf("idempotent retry reported a change: %#v", final)
	}
	issue81AssertFileUnchanged(t, claude, claudeAfter)
	issue81AssertFileUnchanged(t, cursor, cursorAfter)
	issue81AssertFileUnchanged(t, filepath.Join(store, "MEMORY.md"), markerAfter)
}

// E09/R11: a literal relative --dir is persisted as one absolute store in
// every selected writer. The installed commands still meet in that store
// when launched from another working directory.
func TestAdjacentE09InstalledExplicitStoreAcrossClients(t *testing.T) {
	f := newIssue81Fixture(t)
	if runtime.GOOS != "windows" {
		if err := os.Link(f.bin, filepath.Join(filepath.Dir(f.bin), "graymatter")); err != nil {
			t.Fatal(err)
		}
	}
	project := filepath.Join(f.root, "project")
	process := filepath.Join(f.root, "other-cwd")
	for _, dir := range []string{project, process} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	otherStore := f.registerStore(filepath.Join(process, ".graymatter"))
	out := f.run(project, "", "--dir", ".graymatter", "init", "--only", "claudecode,codex,opencode", "--hooks", "--no-path", "--json")
	adjacentSetupJSON(t, out, 0, "complete", "apply")
	wantArgs := []string{"mcp", "serve", "--dir", store}
	clients := adjacentInstalledMCPCommands(t, f, project, wantArgs)
	hookCommand, hookArgs := adjacentManagedHookSpec(t, filepath.Join(project, ".claude", "settings.json"), hooksEventUserPrompt)
	if !adjacentHasDirArg(hookArgs, store) {
		t.Fatalf("installed hook did not pin selected store: %v", hookArgs)
	}
	pathEnv := filepath.Dir(f.bin) + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", pathEnv)
	env := map[string]string{
		"CLAUDE_PROJECT_DIR": project,
		"PATH":               pathEnv,
	}
	m := f.startMCP(process, clients[0].command, clients[0].args, env)
	const fact = "installed explicit routing joins all clients"
	m.add(3, "project", fact)
	m.close()
	for _, clientSpec := range clients[1:] {
		client := f.startMCP(process, clientSpec.command, clientSpec.args, env)
		client.search(3, "project", "explicit routing", fact)
		client.close()
	}
	hook := f.runCommand(hookCommand, process, issue81HookPayload(t, project, "explicit routing"), env, hookArgs...)
	if hook.code != 0 || !strings.Contains(hook.stdout, fact) {
		t.Fatalf("installed hook did not read selected store: %+v", hook)
	}
	for _, leaf := range []string{"gray.db", "hooks.log", "daemon.log"} {
		if info, err := os.Stat(filepath.Join(store, leaf)); err != nil || !info.Mode().IsRegular() {
			t.Fatalf("installed runtime did not write %s in selected store: info=%v err=%v", leaf, info, err)
		}
	}
	if status := f.mustRun(process, "", "--dir", store, "daemon", "status"); !strings.Contains(status.stdout, "daemon: running") {
		t.Fatalf("selected store has no active daemon child: %+v", status)
	}
	addr := runtimeFreeTCPAddress(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	httpArgs := append(append([]string{}, clients[0].args...), "--http", addr)
	httpProcess := exec.CommandContext(ctx, clients[0].command, httpArgs...)
	httpProcess.Dir, httpProcess.Env = process, issue81Env(f.env, env)
	var httpStdout, httpStderr issue81LockedBuffer
	httpProcess.Stdout, httpProcess.Stderr = &httpStdout, &httpStderr
	if err := httpProcess.Start(); err != nil {
		cancel()
		t.Fatalf("start installed HTTP MCP: %v", err)
	}
	t.Cleanup(func() { cancel(); _ = httpProcess.Wait() })
	httpClient := &http.Client{Timeout: time.Second}
	ready := false
	for deadline := time.Now().Add(8 * time.Second); time.Now().Before(deadline); {
		request, err := http.NewRequest(http.MethodPost, "http://"+addr+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		response, err := httpClient.Do(request)
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("installed HTTP MCP changed default auth: status=%d", response.StatusCode)
			}
			ready = true
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !ready {
		t.Fatalf("installed HTTP MCP did not listen: stdout=%q stderr=%q", httpStdout.String(), httpStderr.String())
	}
	token, err := os.ReadFile(filepath.Join(store, "graymatter.http-token"))
	if err != nil || strings.TrimSpace(string(token)) == "" {
		t.Fatalf("selected store has no HTTP token: token=%q err=%v", token, err)
	}
	if _, err := os.Lstat(otherStore); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("installed commands wrote process-cwd store: %v", err)
	}
}

func TestAdjacentE09CustomAbsoluteAndOmittedStore(t *testing.T) {
	for _, tc := range []struct {
		name     string
		explicit bool
	}{
		{name: "custom-absolute", explicit: true},
		{name: "omitted-dynamic"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIssue81Fixture(t)
			if runtime.GOOS != "windows" {
				if err := os.Link(f.bin, filepath.Join(filepath.Dir(f.bin), "graymatter")); err != nil {
					t.Fatal(err)
				}
			}
			project := filepath.Join(f.root, "install-project")
			process := filepath.Join(f.root, "runtime-project")
			for _, dir := range []string{project, process} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			projectStore := f.registerStore(filepath.Join(project, ".graymatter"))
			processStore := f.registerStore(filepath.Join(process, ".graymatter"))
			customStore := filepath.Join(f.root, "custom-absolute-store")
			selectedStore := processStore
			setupArgs := []string{"init", "--only", "claudecode,codex,opencode", "--hooks", "--no-path", "--json"}
			wantArgs := []string{"mcp", "serve"}
			if tc.explicit {
				selectedStore = f.registerStore(customStore)
				setupArgs = append([]string{"--dir", selectedStore}, setupArgs...)
				wantArgs = append(wantArgs, "--dir", selectedStore)
			}
			adjacentSetupJSON(t, f.run(project, "", setupArgs...), 0, "complete", "apply")
			clients := adjacentInstalledMCPCommands(t, f, project, wantArgs)
			hookCommand, hookArgs := adjacentManagedHookSpec(t, filepath.Join(project, ".claude", "settings.json"), hooksEventUserPrompt)
			if tc.explicit {
				if !adjacentHasDirArg(hookArgs, selectedStore) {
					t.Fatalf("absolute store missing from installed hook: %v", hookArgs)
				}
			} else {
				for _, arg := range hookArgs {
					if arg == "--dir" {
						t.Fatalf("omitted --dir became pinned in hook: %v", hookArgs)
					}
				}
			}
			pathEnv := filepath.Dir(f.bin) + string(os.PathListSeparator) + os.Getenv("PATH")
			t.Setenv("PATH", pathEnv)
			env := map[string]string{"PATH": pathEnv}
			fact := tc.name + " fixture routing fact persists"
			m := f.startMCP(process, clients[0].command, clients[0].args, env)
			m.add(3, "runtime-project", fact)
			m.close()
			for _, clientSpec := range clients[1:] {
				client := f.startMCP(process, clientSpec.command, clientSpec.args, env)
				client.search(3, "runtime-project", tc.name+" fixture routing", fact)
				client.close()
			}
			hook := f.runCommand(hookCommand, process, issue81HookPayload(t, process, tc.name+" fixture routing"), env, hookArgs...)
			if hook.code != 0 || !strings.Contains(hook.stdout, fact) {
				t.Fatalf("installed hook did not follow %s store: %+v", tc.name, hook)
			}
			for _, leaf := range []string{"gray.db", "hooks.log", "daemon.log"} {
				if _, err := os.Stat(filepath.Join(selectedStore, leaf)); err != nil {
					t.Fatalf("%s runtime did not write %s in selected store: %v", tc.name, leaf, err)
				}
			}
			if tc.explicit {
				for _, unused := range []string{projectStore, processStore} {
					if _, err := os.Lstat(unused); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("absolute route wrote unselected store %s: %v", unused, err)
					}
				}
			} else {
				issue81AssertOnlyMarker(t, projectStore)
				if _, err := os.Lstat(customStore); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("omitted route wrote unused absolute store: %v", err)
				}
			}
		})
	}
}

// E12: an opt-in client entry remains explicit across a simulated binary
// downgrade. The stub verifies argv preservation and visible rejection of an
// unknown flag; a real older release needs a separate pinned-binary check.
func TestAdjacentE12NoCreateDowngradeIsVisible(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "downgrade-project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	if runtime.GOOS != "windows" {
		if err := os.Link(f.bin, filepath.Join(filepath.Dir(f.bin), "graymatter")); err != nil {
			t.Fatal(err)
		}
	}
	activePATH := filepath.Dir(f.bin) + string(os.PathListSeparator) + os.Getenv("PATH")
	t.Setenv("PATH", activePATH)
	config := filepath.Join(project, ".mcp.json")
	issue81WriteJSON(t, config, map[string]any{"mcpServers": map[string]any{
		"graymatter": map[string]any{"command": "graymatter", "args": []string{"--no-daemon", "mcp", "serve", "--no-create"}},
	}})
	configBefore := issue81SnapshotFile(t, config)
	setup := f.run(project, "", "init", "--no-path", "--only", "claudecode", "--json")
	adjacentSetupJSON(t, setup, 0, "complete", "apply")
	issue81AssertFileUnchanged(t, config, configBefore)
	mcp := issue81MCPFromConfig(t, config)
	activeEnv := map[string]string{"PATH": activePATH, "CLAUDE_PROJECT_DIR": project}
	client := f.startMCP(project, mcp.Command, mcp.Args, activeEnv)
	const fact = "downgrade preserves this prepared store fact"
	client.add(3, "downgrade-project", fact)
	client.search(4, "downgrade-project", "prepared store", fact)
	client.close()
	markerBefore := issue81SnapshotFile(t, filepath.Join(store, "MEMORY.md"))
	dbBefore := issue81SnapshotFile(t, filepath.Join(store, "gray.db"))

	oldDir := filepath.Join(f.root, "old-bin")
	if err := os.MkdirAll(oldDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stubSource := filepath.Join(f.root, "old-cli.go")
	const oldCLI = `package main
import ("fmt"; "os")
func main() {
    for _, arg := range os.Args[1:] {
        if arg == "--no-create" {
            fmt.Fprintln(os.Stderr, "unknown flag: --no-create")
            os.Exit(1)
        }
    }
    fmt.Fprintln(os.Stderr, "unsupported older invocation")
    os.Exit(2)
}`
	if err := os.WriteFile(stubSource, []byte(oldCLI), 0o600); err != nil {
		t.Fatal(err)
	}
	oldBinary := filepath.Join(oldDir, "graymatter")
	if runtime.GOOS == "windows" {
		oldBinary += ".exe"
	}
	build := exec.Command("go", "build", "-o", oldBinary, stubSource)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build downgrade simulator: %v: %s", err, output)
	}
	oldPATH := oldDir + string(os.PathListSeparator) + activePATH
	t.Setenv("PATH", oldPATH)
	old := f.runCommand(mcp.Command, project, "", map[string]string{"PATH": oldPATH, "CLAUDE_PROJECT_DIR": project}, mcp.Args...)
	if old.code != 1 || old.stdout != "" || !strings.Contains(old.stderr, "unknown flag: --no-create") {
		t.Fatalf("downgraded executable did not reject opt-in flag visibly: %+v", old)
	}
	issue81AssertFileUnchanged(t, config, configBefore)
	issue81AssertFileUnchanged(t, filepath.Join(store, "MEMORY.md"), markerBefore)
	issue81AssertFileUnchanged(t, filepath.Join(store, "gray.db"), dbBefore)
	current := f.startMCP(project, f.bin, mcp.Args, activeEnv)
	current.search(3, "downgrade-project", "prepared store", fact)
	current.close()
}

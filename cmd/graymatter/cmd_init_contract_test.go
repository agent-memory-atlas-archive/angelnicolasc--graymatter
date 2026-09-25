package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestInitI01PreservesCustomEntriesForEveryClient(t *testing.T) {
	selection := storeSelection{Path: filepath.Join(t.TempDir(), "memory"), Explicit: true}
	for _, a := range knownAgents(t.TempDir()) {
		t.Run(a.id, func(t *testing.T) {
			var data []byte
			var status string
			var err error
			if a.id == "codex" {
				data = []byte("# owner note\n[mcp_servers.graymatter]\ncommand = \"custom\"\nargs = [\"--secret-canary\"]\n\n[model]\nname = \"keep\"\n")
				_, status, err = planTOMLMCP(data, true, selection, false)
			} else {
				top, entry, jsonc := setupConfigSpec(a.id, selection)
				data = []byte(`{"` + top + `":{"graymatter":{"command":"custom","args":["--secret-canary"],"env":{"TOKEN":"canary"}},"neighbor":{"command":"keep"}}}`)
				_, status, err = planJSONMCP(data, true, top, entry, jsonc, false)
			}
			if err != nil || status != "preserved" {
				t.Fatalf("status=%s err=%v", status, err)
			}
			path := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			before, _ := os.Stat(path)
			p, err := planInitFile(path, 0o600, func(in []byte, exists bool) ([]byte, string, error) {
				if a.id == "codex" {
					return planTOMLMCP(in, exists, selection, false)
				}
				top, entry, jsonc := setupConfigSpec(a.id, selection)
				return planJSONMCP(in, exists, top, entry, jsonc, false)
			})
			if err != nil {
				t.Fatal(err)
			}
			out := p.apply()
			if err := p.close(); err != nil {
				t.Fatal(err)
			}
			if out.status != "preserved" || out.changed || out.err != nil {
				t.Fatalf("%+v", out)
			}
			after, _ := os.Stat(path)
			got, _ := os.ReadFile(path)
			if !bytes.Equal(got, data) || !after.ModTime().Equal(before.ModTime()) || !os.SameFile(before, after) {
				t.Fatal("preserved entry changed")
			}
		})
	}
}

func TestInitI01DocumentStatesForEveryClient(t *testing.T) {
	selection := storeSelection{Path: filepath.Join(t.TempDir(), "store"), Explicit: true}
	for _, agent := range knownAgents(t.TempDir()) {
		t.Run(agent.id, func(t *testing.T) {
			if agent.id == "codex" {
				canonical, status, err := planTOMLMCP(nil, false, selection, false)
				if err != nil || status != "created" {
					t.Fatalf("absent: status=%s err=%v", status, err)
				}
				for _, tc := range []struct {
					name, data, want string
				}{
					{"canonical", string(canonical), "unchanged"},
					{"custom", "[mcp_servers.graymatter]\ncommand = \"custom\"\nargs = [\"--secret\"]\n", "preserved"},
					{"remote", "[mcp_servers.graymatter]\nurl = \"https://example.test/mcp\"\n", "preserved"},
					{"disabled", "[mcp_servers.graymatter]\ncommand = \"graymatter\"\nenabled = false\n", "preserved"},
					{"extras", string(canonical) + "owner = \"team\"\n", "preserved"},
				} {
					t.Run(tc.name, func(t *testing.T) {
						out, got, err := planTOMLMCP([]byte(tc.data), true, selection, false)
						if err != nil || got != tc.want || !bytes.Equal(out, []byte(tc.data)) {
							t.Fatalf("status=%s err=%v", got, err)
						}
					})
				}
				return
			}
			top, entry, jsonc := setupConfigSpec(agent.id, selection)
			canonical, status, err := planJSONMCP(nil, false, top, entry, jsonc, false)
			if err != nil || status != "created" {
				t.Fatalf("absent: status=%s err=%v", status, err)
			}
			entryWithExtra := map[string]any{}
			for key, value := range entry {
				entryWithExtra[key] = value
			}
			entryWithExtra["owner"] = "team"
			extraJSON, _ := json.Marshal(entryWithExtra)
			for _, tc := range []struct {
				name, data, want string
			}{
				{"canonical", string(canonical), "unchanged"},
				{"custom", `{"` + top + `":{"graymatter":{"command":"custom","args":["--secret"]}}}`, "preserved"},
				{"remote", `{"` + top + `":{"graymatter":{"type":"http","url":"https://example.test/mcp","headers":{"Authorization":"private"}}}}`, "preserved"},
				{"disabled", `{"` + top + `":{"graymatter":{"command":"graymatter","enabled":false}}}`, "preserved"},
				{"extras", `{"` + top + `":{"graymatter":` + string(extraJSON) + `}}`, "preserved"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					out, got, err := planJSONMCP([]byte(tc.data), true, top, entry, jsonc, false)
					if err != nil || got != tc.want || !bytes.Equal(out, []byte(tc.data)) {
						t.Fatalf("status=%s err=%v", got, err)
					}
				})
			}
		})
	}
}

func TestInitI02JSONReplaceOnlyTarget(t *testing.T) {
	selection := storeSelection{Path: filepath.Join(t.TempDir(), "store"), Explicit: true}
	before := []byte("// owner\r\n{\r\n  \"mcp\": {\r\n    \"other\": {\"url\": \"https://site.test/a//b\"}, // keep\r\n    \"graymatter\": {\"type\": \"remote\", \"headers\": {\"Authorization\": \"canary\"}}\r\n  },\r\n  \"model\": \"keep\"\r\n}\r\n")
	top, entry, jsonc := setupConfigSpec("opencode", selection)
	next, status, err := planJSONMCP(before, true, top, entry, jsonc, true)
	if err != nil || status != "updated" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	if !bytes.Contains(next, []byte("// owner\r\n")) || !bytes.Contains(next, []byte(`"other": {"url": "https://site.test/a//b"}, // keep`)) || !bytes.Contains(next, []byte(`"model": "keep"`)) || bytes.Contains(next, []byte("canary")) {
		t.Fatalf("unexpected edited document: %s", next)
	}
	_, status, err = planJSONMCP(next, true, top, entry, jsonc, true)
	if err != nil || status != "unchanged" {
		t.Fatalf("rerun status=%s err=%v", status, err)
	}
}

func TestInitI02TOMLReplaceOnlyTarget(t *testing.T) {
	selection := storeSelection{Path: filepath.Join(t.TempDir(), "store"), Explicit: true}
	before := []byte("# owner note\r\n[mcp_servers.graymatter]\r\ncommand = \"custom\"\r\nargs = [\"--secret-canary\"]\r\n[mcp_servers.graymatter.env]\r\nTOKEN = \"canary\"\r\n[mcp_servers.other]\r\ncommand = \"keep\"\r\n[model]\r\nname = \"keep\"\r\n")
	out, status, err := planTOMLMCP(before, true, selection, true)
	if err != nil || status != "updated" {
		t.Fatalf("replace status=%s err=%v", status, err)
	}
	if !bytes.HasPrefix(out, []byte("# owner note\r\n")) ||
		!bytes.Contains(out, []byte("[mcp_servers.other]\r\ncommand = \"keep\"\r\n[model]\r\nname = \"keep\"\r\n")) ||
		bytes.Contains(out, []byte("canary")) {
		t.Fatalf("replacement changed neighbors or retained target data: %s", out)
	}
	again, status, err := planTOMLMCP(out, true, selection, true)
	if err != nil || status != "unchanged" || !bytes.Equal(again, out) {
		t.Fatalf("rerun status=%s err=%v", status, err)
	}
}

func TestInitI03RejectsMalformedShapesAndEncoding(t *testing.T) {
	selection := storeSelection{}
	entry := map[string]any{"command": "graymatter", "args": []string{"mcp", "serve"}}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"empty", []byte{}}, {"root array", []byte(`[]`)}, {"parent scalar", []byte(`{"mcpServers":false}`)},
		{"target scalar", []byte(`{"mcpServers":{"graymatter":false}}`)},
		{"duplicate", []byte(`{"mcpServers":{},"mcpServers":{}}`)},
		{"invalid", []byte(`{"mcpServers":`)}, {"bom", []byte{0xef, 0xbb, 0xbf, '{', '}'}},
		{"oversize", bytes.Repeat([]byte{'x'}, initConfigLimit+1)},
		{"deep", []byte(`{"mcpServers":{"graymatter":` + strings.Repeat("[", initJSONMaxDepth+1) +
			"0" + strings.Repeat("]", initJSONMaxDepth+1) + `}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, tc.data, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := planInitFile(path, 0o600, func(data []byte, exists bool) ([]byte, string, error) {
				return planJSONMCP(data, exists, "mcpServers", entry, false, false)
			})
			if err == nil {
				t.Fatal("wanted preflight error")
			}
			got, _ := os.ReadFile(path)
			if !bytes.Equal(got, tc.data) {
				t.Fatal("preflight modified file")
			}
		})
	}
	_ = selection
}

func TestInitI04TOMLQuotedTableAndUnsupportedInline(t *testing.T) {
	selection := storeSelection{Path: filepath.Join(t.TempDir(), "store"), Explicit: true}
	data := []byte("# outer\r\n[mcp_servers.\"graymatter\"]\r\ncommand = \"custom\"\r\nargs = [\"x\"]\r\n[mcp_servers.graymatter.env]\r\nTOKEN = \"canary\"\r\n[model]\r\nname = '''line one\r\n[line two]\r\n'''\r\n")
	next, status, err := planTOMLMCP(data, true, selection, true)
	if err != nil || status != "updated" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	if !bytes.Contains(next, []byte("# outer\r\n")) || !bytes.Contains(next, []byte("[model]\r\nname = '''line one\r\n[line two]\r\n'''")) || bytes.Contains(next, []byte("canary")) {
		t.Fatalf("TOML lost unrelated content: %s", next)
	}
	inline := []byte("mcp_servers = { graymatter = { command = \"custom\" } }\n")
	_, status, err = planTOMLMCP(inline, true, selection, false)
	if err != nil || status != "preserved" {
		t.Fatalf("inline preserved status=%s err=%v", status, err)
	}
	_, _, err = planTOMLMCP(inline, true, selection, true)
	if err == nil || err.Error() != "unsupported_edit" {
		t.Fatalf("inline replacement error=%v", err)
	}
}

func TestInitI04TOMLEscapedTripleQuoteKeepsTextualHeader(t *testing.T) {
	data := []byte(`payload = """
line \"""
[mcp_servers.graymatter]
fake = true
"""
[mcp_servers.graymatter]
command = "custom"
`)
	selection := storeSelection{Path: filepath.Join(t.TempDir(), "store"), Explicit: true}
	out, status, err := planTOMLMCP(data, true, selection, true)
	if err != nil || status != "updated" {
		t.Fatalf("escaped multiline replacement: status=%s err=%v", status, err)
	}
	if !bytes.Contains(out, []byte(`line \"""
[mcp_servers.graymatter]
fake = true
"""`)) || bytes.Contains(out, []byte(`command = "custom"`)) {
		t.Fatalf("multiline text was changed: %s", out)
	}
}

func TestInitI04HooksPreserveForeignBytesAndLargeNumbers(t *testing.T) {
	selection := storeSelection{Path: filepath.Join(t.TempDir(), "store"), Explicit: true}
	foreign := `{"matcher":"foreign","hooks":[{"type":"command","command":"other","timeout":9007199254740993}]}`
	before := []byte("{\r\n  \"owner\" : {\"precision\":9007199254740993},\r\n  \"hooks\": {\"OtherEvent\": [" + foreign + "], \"UserPromptSubmit\": [" + foreign + "]}\r\n}\r\n")
	beforeOutside := []byte("\"owner\" : {\"precision\":9007199254740993},\r\n")
	out, status, err := renderInitHookSettings(before, true, "graymatter.exe", selection)
	if err != nil || status != "updated" {
		t.Fatalf("status=%s err=%v", status, err)
	}
	if !bytes.Contains(out, beforeOutside) || bytes.Count(out, []byte(foreign)) != 2 ||
		!bytes.Contains(out, []byte(`"OtherEvent": [`+foreign+`]`)) {
		t.Fatalf("foreign settings changed: %s", out)
	}
	again, status, err := renderInitHookSettings(out, true, "graymatter.exe", selection)
	if err != nil || status != "unchanged" || !bytes.Equal(again, out) {
		t.Fatalf("rerun changed hook settings: status=%s err=%v", status, err)
	}
}

func TestInitI04HooksPreserveForeignInterstices(t *testing.T) {
	selection := storeSelection{Path: filepath.Join(t.TempDir(), "store"), Explicit: true}
	exe := "graymatter.exe"
	owned, err := json.Marshal(hookGroupsForPolicySelection(exe, hooksEventUserPrompt, scopeProject, "", selection)[0])
	if err != nil {
		t.Fatal(err)
	}
	foreign := `{"matcher":"foreign","hooks":[{"type":"command","command":"other","timeout":9007199254740993}]}`
	before := []byte(`{"hooks":{"UserPromptSubmit": [` + "\r\n  " + string(owned) + ",\r\n  " + foreign + ",\r\n  " + string(owned) + "\r\n]}}")
	out, status, err := renderInitHookSettings(before, true, exe, selection)
	if err != nil || status != "updated" {
		t.Fatalf("hook rewrite: status=%s err=%v", status, err)
	}
	wantArray := []byte("[\r\n  \r\n  " + foreign + "," + string(owned) + "\r\n  \r\n]")
	if !bytes.Contains(out, wantArray) {
		t.Fatalf("foreign group separators or whitespace changed: %s", out)
	}
	again, status, err := renderInitHookSettings(out, true, exe, selection)
	if err != nil || status != "unchanged" || !bytes.Equal(again, out) {
		t.Fatalf("hook rewrite not idempotent: status=%s err=%v", status, err)
	}
}

func TestInitI05WizardSelectionAndPathConsent(t *testing.T) {
	agents := knownAgents(t.TempDir())
	for _, input := range []string{"0,1", "1,x", "99"} {
		if _, valid := parseWizardSelection(input, agents); valid {
			t.Fatalf("%q accepted", input)
		}
	}
	for _, input := range []string{"", "0"} {
		if selected, valid := parseWizardSelection(input, agents); !valid || len(selected) != 0 {
			t.Fatalf("%q was not None", input)
		}
	}
	defer wireInteractiveTest("0,1\n0\n")()
	selected, path, err := collectInitWizardSelection(agents, true, true, true, true)
	if err != nil || len(selected) != 0 || path {
		t.Fatalf("selection=%v path=%v err=%v", selected, path, err)
	}
}

func TestInitI05WizardEOFAndWindowsPATH(t *testing.T) {
	agents := knownAgents(t.TempDir())
	for _, tc := range []struct {
		name, input string
		noPath      bool
		wantPath    bool
		wantCancel  bool
	}{
		{"selection EOF", "", false, false, true},
		{"invalid selection reprompt", "0,1\n0\n", false, false, false},
		{"PATH default No", "0\n\n", false, false, false},
		{"PATH EOF No", "0\n", false, false, false},
		{"PATH explicit Yes", "0\ny\n", false, runtime.GOOS == "windows", false},
		{"PATH invalid reprompt", "0\nmaybe\nyes\n", false, runtime.GOOS == "windows", false},
		{"no-path skips question", "0\ny\n", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cleanup := wireInteractiveTest(tc.input)
			defer cleanup()
			selected, path, err := collectInitWizardSelection(agents, tc.noPath, false, false, false)
			if (err != nil) != tc.wantCancel || path != tc.wantPath || (!tc.wantCancel && len(selected) != 0) {
				t.Fatalf("selection=%v path=%v err=%v", selected, path, err)
			}
		})
	}
}

func TestInitI06PreflightAllBeforeStore(t *testing.T) {
	project := t.TempDir()
	store := filepath.Join(project, "memory")
	if err := os.MkdirAll(filepath.Join(project, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(project, ".cursor", "mcp.json")
	if err := os.WriteFile(bad, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	opts := initOptions{selection: storeSelection{Path: store}, projectDir: project, selected: map[string]bool{"claudecode": true, "cursor": true}}
	result := newSetupResult(false)
	if err := planInit(opts, &result); err == nil {
		t.Fatal("wanted preflight error")
	}
	closeInitPlans(&result)
	for _, action := range result.Actions {
		if action.Status == "created" || action.Status == "updated" || action.Status == "unchanged" || action.Status == "preserved" {
			t.Fatalf("preflight claimed unapplied action %s=%s", action.ID, action.Status)
		}
	}
	for _, path := range []string{store, filepath.Join(project, ".mcp.json")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("preflight created %s", path)
		}
	}
}

func TestInitI06PATHPreflightStopsAllWrites(t *testing.T) {
	project := t.TempDir()
	store := filepath.Join(project, "store")
	previous := initPreflightPath
	initPreflightPath = func() error { return errors.New("unreadable registry value") }
	defer func() { initPreflightPath = previous }()
	opts := initOptions{selection: storeSelection{Path: store}, projectDir: project,
		selected: map[string]bool{"claudecode": true}, path: true}
	result := newSetupResult(false)
	if err := planInit(opts, &result); err == nil {
		t.Fatal("PATH preflight unexpectedly accepted")
	}
	closeInitPlans(&result)
	if _, err := os.Stat(store); !os.IsNotExist(err) {
		t.Fatalf("store changed after PATH preflight failure: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("client config changed after PATH preflight failure: %v", err)
	}
	for _, action := range result.Actions {
		if action.ID == "path" && (action.Status != "failed" || action.ErrorCode == nil) {
			t.Fatalf("PATH failure missing from receipt: %+v", action)
		}
	}
}

type initFailReader struct{}

func (initFailReader) Read([]byte) (int, error) { return 0, errors.New("random unavailable") }

func TestInitI09ParentReceiptSurvivesStageFailure(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "new", "nested", "config.json")
	plan, err := planInitFile(path, 0o600, func([]byte, bool) ([]byte, string, error) {
		return []byte("{}\n"), "created", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	previous := initStageRandom
	initStageRandom = initFailReader{}
	defer func() { initStageRandom = previous }()
	out := plan.apply()
	if out.status != "failed" || !out.changed || out.err == nil || out.uncertain {
		t.Fatalf("stage failure lost mkdir receipt: %+v", out)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("owned parent was not created: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("target was published after stage failure: %v", err)
	}
}

func TestInitI09AmbiguousMkdirReceipt(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "new", "config.json")
	plan, err := planInitFile(path, 0o600, func([]byte, bool) ([]byte, string, error) {
		return []byte("{}\n"), "created", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	previous := initMakeDir
	initMakeDir = func(root *os.Root, name string, mode os.FileMode) error {
		if err := previous(root, name, mode); err != nil {
			return err
		}
		return errors.New("injected error after mkdir")
	}
	defer func() { initMakeDir = previous }()
	out := plan.apply()
	if out.status != "failed" || !out.uncertain || out.err == nil {
		t.Fatalf("ambiguous mkdir was reported as certain: %+v", out)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("target published after mkdir error: %v", err)
	}
}

func TestInitI09RemovedMkdirStillUncertain(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "new", "config.json")
	plan, err := planInitFile(path, 0o600, func([]byte, bool) ([]byte, string, error) {
		return []byte("{}\n"), "created", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	previous := initMakeDir
	initMakeDir = func(root *os.Root, name string, mode os.FileMode) error {
		if err := previous(root, name, mode); err != nil {
			return err
		}
		if err := root.Remove(name); err != nil {
			return err
		}
		return errors.New("injected mkdir error after create and removal")
	}
	defer func() { initMakeDir = previous }()
	out := plan.apply()
	if out.status != "failed" || !out.uncertain || out.err == nil {
		t.Fatalf("removed mkdir was falsely certain: %+v", out)
	}
}

func TestInitI07PATHErrorIsUncertainEvenWithBestEffort(t *testing.T) {
	project := t.TempDir()
	opts := initOptions{selection: storeSelection{Path: filepath.Join(project, "store")},
		projectDir: project, selected: map[string]bool{}, path: true, bestEffort: true}
	result := newSetupResult(true)
	if err := planInit(opts, &result); err != nil {
		t.Fatal(err)
	}
	previous := initAddExeDirToUserPath
	initAddExeDirToUserPath = func() (bool, error) { return false, errors.New("ambiguous registry write") }
	defer func() { initAddExeDirToUserPath = previous }()
	applyInit(opts, &result)
	closeInitPlans(&result)
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	if err := renderSetupResult(command, &result, true, false); err == nil || !result.EffectsUncertain || result.Status != "partial" {
		t.Fatalf("ambiguous PATH result: %+v err=%v", result, err)
	}
	if bytes.Contains(output.Bytes(), []byte("store: created")) || !bytes.Contains(output.Bytes(), []byte("path_update_failed")) {
		t.Fatalf("quiet output leaked success or hid failure: %s", output.String())
	}
}

func TestInitI09NewParentAliasRejected(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "new", "config.json")
	plan, err := planInitFile(path, 0o600, func([]byte, bool) ([]byte, string, error) {
		return []byte("{}\n"), "created", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	other := filepath.Join(base, "other")
	if err := os.Mkdir(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, filepath.Join(base, "new")); err != nil {
		t.Skipf("fixture filesystem cannot create directory alias: %v", err)
	}
	out := plan.apply()
	if out.status != "failed" || out.err == nil || out.uncertain {
		t.Fatalf("new parent alias accepted: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(other, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("external target was modified: %v", err)
	}
}

func TestInitI07PartialApplyAndRetry(t *testing.T) {
	project := t.TempDir()
	opts := initOptions{selection: storeSelection{Path: filepath.Join(project, "memory")}, projectDir: project,
		selected: map[string]bool{"claudecode": true, "cursor": true}}
	result := newSetupResult(false)
	if err := planInit(opts, &result); err != nil {
		t.Fatal(err)
	}
	original := applyInitFile
	applyInitFile = func(p *initFilePlan) initPublishResult {
		if strings.HasSuffix(p.path, filepath.Join(".cursor", "mcp.json")) {
			return initPublishResult{status: "failed", err: errors.New("injected")}
		}
		return original(p)
	}
	applyInit(opts, &result)
	applyInitFile = original
	closeInitPlans(&result)
	result.finalize()
	if result.Status != "partial" || !result.Changed || result.EffectsUncertain {
		t.Fatalf("%+v", result)
	}
	if _, err := os.Stat(filepath.Join(project, ".mcp.json")); err != nil {
		t.Fatalf("first writer lost: %v", err)
	}
	if _, err := os.Stat(filepath.Join(project, ".cursor", "mcp.json")); !os.IsNotExist(err) {
		t.Fatal("failed writer published")
	}
	retry := newSetupResult(false)
	if err := planInit(opts, &retry); err != nil {
		t.Fatal(err)
	}
	applyInit(opts, &retry)
	closeInitPlans(&retry)
	retry.finalize()
	if retry.Status != "complete" {
		t.Fatalf("retry status=%s errors=%v", retry.Status, retry.Errors)
	}
}

func TestInitI07CloseFailureKeepsPublishedStatus(t *testing.T) {
	project := t.TempDir()
	opts := initOptions{selection: storeSelection{Path: filepath.Join(project, "store")},
		projectDir: project, selected: map[string]bool{"claudecode": true}}
	result := newSetupResult(false)
	if err := planInit(opts, &result); err != nil {
		t.Fatal(err)
	}
	applyInit(opts, &result)
	previous := closeInitFile
	closeInitFile = func(plan *initFilePlan) error {
		_ = previous(plan)
		if strings.HasSuffix(plan.path, ".mcp.json") {
			return errors.New("close failed after publication")
		}
		return nil
	}
	defer func() { closeInitFile = previous }()
	closeInitPlans(&result)
	result.finalize()
	if !result.EffectsUncertain || result.Status != "partial" {
		t.Fatalf("close failure receipt: %+v", result)
	}
	for _, action := range result.Actions {
		if action.ID == "mcp:claudecode" &&
			(action.Status != "applied_cleanup_failed" || !action.Changed || action.ErrorCode == nil) {
			t.Fatalf("published action misreported: %+v", action)
		}
	}
	if _, err := os.Stat(filepath.Join(project, ".mcp.json")); err != nil {
		t.Fatalf("published config disappeared: %v", err)
	}
}

func TestInitI09RejectsUnsafeLeafAndHardlink(t *testing.T) {
	project := t.TempDir()
	target := filepath.Join(project, "target.json")
	if err := os.WriteFile(target, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(project, "alias.json")
	if err := os.Link(target, alias); err != nil {
		t.Fatalf("fixture cannot create hardlink: %v", err)
	}
	_, err := planInitFile(alias, 0o600, func(data []byte, exists bool) ([]byte, string, error) {
		return planJSONMCP(data, exists, "mcpServers", mcpEntry, false, false)
	})
	if err == nil || err.Error() != "unsupported_metadata" {
		t.Fatalf("hardlink mutation error=%v", err)
	}
	link := filepath.Join(project, "link.json")
	if err := os.Symlink(target, link); err == nil {
		_, err := planInitFile(link, 0o600, func(data []byte, exists bool) ([]byte, string, error) { return data, "preserved", nil })
		if err == nil || err.Error() != "unsafe_leaf" {
			t.Fatalf("symlink error=%v", err)
		}
	} else {
		// Restricted Windows tokens may be unable to create symlinks. A
		// directory leaf still checks the regular-file gate on that host.
		if err := os.Mkdir(link, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := planInitFile(link, 0o600, func(data []byte, exists bool) ([]byte, string, error) { return data, "preserved", nil })
		if err == nil || err.Error() != "unsafe_leaf" {
			t.Fatalf("nonregular leaf error=%v", err)
		}
	}
}

func TestInitI10AbsentPublicationConflict(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	render := func(data []byte, exists bool) ([]byte, string, error) { return []byte("{}\n"), "created", nil }
	first, err := planInitFile(path, 0o600, render)
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	second, err := planInitFile(path, 0o600, render)
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	a, b := first.apply(), second.apply()
	if a.status != "created" || b.status != "failed" || b.changed != true || b.uncertain {
		t.Fatalf("first=%+v second=%+v", a, b)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "{}\n" {
		t.Fatalf("target changed: %s", got)
	}
}

func TestInitI10ConcurrentExistingFileGuardAcrossRoots(t *testing.T) {
	initAllowAuditSACLForTest(t)
	parent := filepath.Join(t.TempDir(), "settings")
	if err := os.MkdirAll(filepath.Join(parent, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(parent, "config.json")
	before := []byte(`{"owner":"before"}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	initSetStableReplacementMetadataForTest(t, path)
	first, err := planInitFile(path, 0o600, func([]byte, bool) ([]byte, string, error) {
		return []byte(`{"owner":"first"}`), "updated", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	// Retain the different lexical spelling so each plan opens its own root.
	alias := parent + string(filepath.Separator) + "sub" + string(filepath.Separator) + ".." + string(filepath.Separator) + "config.json"
	second, err := planInitFile(alias, 0o600, func([]byte, bool) ([]byte, string, error) {
		return []byte(`{"owner":"second"}`), "updated", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.close()
	if first.root == second.root {
		t.Fatal("plans unexpectedly share a root handle")
	}

	previous := initAcquireFileGuard
	entered := make(chan struct{})
	resume := make(chan struct{})
	var resumed sync.Once
	release := func() { resumed.Do(func() { close(resume) }) }
	defer release()
	var firstGuard atomic.Bool
	initAcquireFileGuard = func(file *os.File, info fs.FileInfo) (func() error, error) {
		unlock, err := previous(file, info)
		if err == nil && firstGuard.CompareAndSwap(false, true) {
			close(entered)
			<-resume
		}
		return unlock, err
	}
	defer func() { initAcquireFileGuard = previous }()

	firstDone := make(chan struct{})
	var firstOut initPublishResult
	go func() {
		firstOut = first.apply()
		close(firstDone)
	}()
	defer func() {
		release()
		select {
		case <-firstDone:
		case <-time.After(20 * time.Second):
			t.Error("first init did not stop during test cleanup")
		}
	}()
	select {
	case <-entered:
	case <-firstDone:
		t.Fatalf("first init failed before file guard: %+v; err=%v (%T)", firstOut, firstOut.err, firstOut.err)
	case <-time.After(20 * time.Second):
		t.Fatal("first init did not acquire the file guard")
	}
	blocked := second.apply()
	if blocked.status != "failed" || blocked.err == nil || !strings.Contains(blocked.err.Error(), "initialization_busy") {
		t.Fatalf("second init bypassed held guard: %+v; err=%v", blocked, blocked.err)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(data, before) {
		t.Fatalf("target changed while first init was held: %s err=%v", data, err)
	}
	release()
	select {
	case <-firstDone:
		if firstOut.status != "updated" || firstOut.err != nil {
			t.Fatalf("first init failed: %+v; err=%v", firstOut, firstOut.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("first init did not complete after guard release")
	}
	stale := second.apply()
	if stale.status != "failed" || stale.err == nil || !strings.Contains(stale.err.Error(), "publication_conflict") {
		t.Fatalf("stale second plan overwrote publication: %+v; err=%v", stale, stale.err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != `{"owner":"first"}` {
		t.Fatalf("target changed after conflict: %s err=%v", data, err)
	}
}

func TestInitI10GuardReleaseErrorKeepsPublicationReceipt(t *testing.T) {
	initAllowAuditSACLForTest(t)
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"owner":"before"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	initSetStableReplacementMetadataForTest(t, path)
	plan, err := planInitFile(path, 0o600, func([]byte, bool) ([]byte, string, error) {
		return []byte(`{"owner":"after"}`), "updated", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	previous := initAcquireFileGuard
	initAcquireFileGuard = func(file *os.File, info fs.FileInfo) (func() error, error) {
		unlock, err := previous(file, info)
		if err != nil {
			return nil, err
		}
		return func() error {
			_ = unlock()
			return errors.New("injected guard release error")
		}, nil
	}
	defer func() { initAcquireFileGuard = previous }()
	out := plan.apply()
	if out.status != "applied_cleanup_failed" || !out.changed || !out.uncertain || out.err == nil {
		t.Fatalf("lost confirmed publication after guard release error: %+v; err=%v", out, out.err)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != `{"owner":"after"}` {
		t.Fatalf("confirmed publication missing: %s err=%v", data, err)
	}
}

func TestInitI10PhysicalDestinationAliases(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "settings")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(parent, "config.json")
	alias := filepath.Join(parent, "..", "settings", "config.json")
	same, err := initSameDestinationPaths(first, alias)
	if err != nil || !same {
		t.Fatalf("lexical alias not detected: same=%v err=%v", same, err)
	}
	if err := os.WriteFile(first, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	same, err = initSameDestinationPaths(first, alias)
	if err != nil || !same {
		t.Fatalf("physical alias not detected: same=%v err=%v", same, err)
	}
	other := filepath.Join(parent, "other.json")
	same, err = initSameDestinationPaths(first, other)
	if err != nil || same {
		t.Fatalf("distinct destination conflated: same=%v err=%v", same, err)
	}
}

func TestInitI10StoreAliasRetargetRejected(t *testing.T) {
	base := t.TempDir()
	first := filepath.Join(base, "first")
	second := filepath.Join(base, "second")
	for _, path := range []string{first, second} {
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(base, "selected")
	if err := os.Symlink(first, alias); err != nil {
		// The fallback exercises the same physical-identity guard when the
		// fixture filesystem cannot create a directory alias.
		alias = first
	}
	plan, err := planInitStore(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	if alias != first {
		if err := os.Remove(alias); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(second, alias); err != nil {
			t.Fatal(err)
		}
	} else {
		plan.path = second // Equivalent path-to-inode mismatch under held root.
	}
	receipt, err := prepareInitStoreWithReceipt(plan)
	if err == nil || receipt.changed {
		t.Fatalf("retargeted store alias accepted: receipt=%+v err=%v", receipt, err)
	}
	for _, path := range []string{first, second} {
		if _, err := os.Stat(filepath.Join(path, "MEMORY.md")); !os.IsNotExist(err) {
			t.Fatalf("marker published under %s: %v", path, err)
		}
	}
}

func TestInitI10AmbiguousStoreMkdirReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "store")
	plan, err := planInitStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	previous := initMakeDir
	initMakeDir = func(root *os.Root, name string, mode os.FileMode) error {
		if err := previous(root, name, mode); err != nil {
			return err
		}
		return errors.New("injected error after mkdir")
	}
	defer func() { initMakeDir = previous }()
	receipt, err := prepareInitStoreWithReceipt(plan)
	if err == nil || !receipt.uncertain || receipt.status != "" {
		t.Fatalf("ambiguous store mkdir receipt: %+v err=%v", receipt, err)
	}
	if _, err := os.Stat(filepath.Join(path, "MEMORY.md")); !os.IsNotExist(err) {
		t.Fatalf("marker published after mkdir error: %v", err)
	}
}

func TestInitI10RemovedStoreStageStillUncertain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	plan, err := planInitStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	previous := initStoreOpsFactory
	initStoreOpsFactory = func() storeInitOps {
		ops := previous()
		open := ops.openFile
		ops.openFile = func(root *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
			file, err := open(root, name, flag, perm)
			if err != nil {
				return nil, err
			}
			_ = file.Close()
			_ = root.Remove(name)
			return nil, errors.New("injected stage error after create and removal")
		}
		return ops
	}
	defer func() { initStoreOpsFactory = previous }()
	receipt, err := prepareInitStoreWithReceipt(plan)
	if err == nil || !receipt.uncertain {
		t.Fatalf("removed stage was falsely certain: %+v err=%v", receipt, err)
	}
	if _, err := os.Stat(filepath.Join(path, "MEMORY.md")); !os.IsNotExist(err) {
		t.Fatalf("marker published after stage failure: %v", err)
	}
}

func TestInitI09ExistingReplacementPreservesMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	before := []byte(`{"mcpServers":{"graymatter":{"command":"custom","args":["--secret-canary"]},"other":{"command":"keep"}}}`)
	if err := os.WriteFile(path, before, 0o600); err != nil {
		t.Fatal(err)
	}
	oldInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	selection := storeSelection{Path: filepath.Join(t.TempDir(), "store"), Explicit: true}
	plan, err := planInitFile(path, 0o600, func(data []byte, exists bool) ([]byte, string, error) {
		return planJSONMCP(data, exists, "mcpServers", mcpEntryFor(selection), false, true)
	})
	if runtime.GOOS == "windows" && err != nil && err.Error() == "unsupported_metadata" {
		// Under an ordinary token Windows does not expose the audit SACL.
		// The safe outcome is refusal before any stage or target write.
		current, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(current, before) {
			t.Fatalf("rejected replacement changed target: %v", readErr)
		}
		currentInfo, statErr := os.Stat(path)
		if statErr != nil || !os.SameFile(oldInfo, currentInfo) || !oldInfo.ModTime().Equal(currentInfo.ModTime()) {
			t.Fatalf("rejected replacement changed metadata: %v", statErr)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	out := plan.apply()
	if err := plan.close(); err != nil {
		t.Fatal(err)
	}
	if out.err != nil || out.status != "updated" {
		t.Fatalf("publication: %+v", out)
	}
	newInfo, _ := os.Stat(path)
	if oldInfo.Mode().Perm() != newInfo.Mode().Perm() {
		t.Fatalf("mode changed: %v -> %v", oldInfo.Mode(), newInfo.Mode())
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("canary")) || !bytes.Contains(data, []byte(`"other":{"command":"keep"}`)) {
		t.Fatalf("replacement changed neighbors or retained old target: %s", data)
	}
}

func TestInitI09RenameErrorReceiptsAllPlatforms(t *testing.T) {
	for _, publishFirst := range []bool{false, true} {
		name := "before publication"
		if publishFirst {
			name = "after publication"
		}
		t.Run(name, func(t *testing.T) {
			initAllowAuditSACLForTest(t)
			path := filepath.Join(t.TempDir(), "config.json")
			before := []byte(`{"mcpServers":{}}`)
			if err := os.WriteFile(path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			initSetStableReplacementMetadataForTest(t, path)
			plan, err := planInitFile(path, 0o600, func(data []byte, exists bool) ([]byte, string, error) {
				return planJSONMCP(data, exists, "mcpServers", mcpEntry, false, false)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer plan.close()
			previous := initReplacePublishedFile
			initReplacePublishedFile = func(root *os.Root, stage, target string) error {
				if publishFirst {
					if err := previous(root, stage, target); err != nil {
						return err
					}
				}
				return errors.New("injected rename error")
			}
			defer func() { initReplacePublishedFile = previous }()
			out := plan.apply()
			want := "failed"
			if publishFirst {
				want = "applied_cleanup_failed"
			}
			if out.status != want || out.err == nil || out.uncertain {
				t.Fatalf("rename receipt: %+v; err=%v", out, out.err)
			}
			data, err := os.ReadFile(path)
			if err != nil || (bytes.Equal(data, before) == publishFirst) {
				t.Fatalf("publication does not match receipt: data=%s err=%v", data, err)
			}
		})
	}
}

func TestInitI10ParentMoveKeepsPublicationAnchored(t *testing.T) {
	base := t.TempDir()
	oldParent := filepath.Join(base, "before")
	newParent := filepath.Join(base, "after")
	if err := os.Mkdir(oldParent, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(oldParent, "config.json")
	wantStatus := "created"
	if runtime.GOOS != "windows" {
		if err := os.WriteFile(path, []byte(`{"mcpServers":{}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		wantStatus = "updated"
	}
	plan, err := planInitFile(path, 0o600, func(data []byte, exists bool) ([]byte, string, error) {
		return planJSONMCP(data, exists, "mcpServers", mcpEntry, false, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.close()
	if err := os.Rename(oldParent, newParent); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		// A held directory can block renaming on Windows. Publication then
		// remains anchored in the original directory.
		out := plan.apply()
		if out.err != nil || out.status != wantStatus {
			t.Fatalf("publication after blocked move: %+v", out)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("original directory lost: %v", err)
		}
		return
	}
	out := plan.apply()
	if out.err != nil || out.status != wantStatus {
		t.Fatalf("anchored publication: %+v", out)
	}
	if _, err := os.Stat(filepath.Join(oldParent, "config.json")); !os.IsNotExist(err) {
		t.Fatalf("old path recreated: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(newParent, "config.json"))
	if err != nil || !bytes.Contains(data, []byte(`"graymatter"`)) {
		t.Fatalf("moved directory not updated: %s %v", data, err)
	}
}

func mcpEntryFor(selection storeSelection) map[string]any {
	return map[string]any{"command": "graymatter", "args": mcpServeArgs(selection)}
}

func TestInitI11RoutingExplicitness(t *testing.T) {
	cwd := t.TempDir()
	for _, tc := range []struct {
		dir     string
		changed bool
		want    []string
	}{
		{".graymatter", false, []string{"mcp", "serve"}},
		{".graymatter", true, []string{"mcp", "serve", "--dir", filepath.Join(cwd, ".graymatter")}},
		{" custom ", true, []string{"mcp", "serve", "--dir", filepath.Join(cwd, " custom ")}},
	} {
		selection, err := resolveStoreSelection(tc.dir, tc.changed, cwd)
		if err != nil || !reflect.DeepEqual(mcpServeArgs(selection), tc.want) {
			t.Fatalf("%q: %v %v", tc.dir, mcpServeArgs(selection), err)
		}
		hookArgs := hookRunArgs(selection, "session-start")
		if selection.Explicit {
			if !reflect.DeepEqual(hookArgs[len(hookArgs)-2:], []string{"--dir", selection.Path}) {
				t.Fatalf("explicit hooks lost selected store: %v", hookArgs)
			}
		} else if len(hookArgs) != 4 {
			t.Fatalf("default hooks became pinned: %v", hookArgs)
		}
	}
	if _, err := resolveStoreSelection(" \t ", true, cwd); err == nil {
		t.Fatal("whitespace directory accepted")
	}
	absolute := filepath.Join(cwd, "absolute-store")
	if got, err := resolveStoreSelection(absolute, true, ""); err != nil || got.Path != absolute || !got.Explicit {
		t.Fatalf("absolute directory with unavailable cwd: %+v %v", got, err)
	}
}

func TestInitI08JSONResultSanitizesErrors(t *testing.T) {
	secret := "secret-canary-9007199254740993"
	_, _, parseErr := planJSONMCP([]byte(`{"mcpServers":{"graymatter":{"command":"`+secret+`"}},`),
		true, "mcpServers", mcpEntry, false, false)
	if parseErr == nil {
		t.Fatal("canary fixture was not malformed")
	}
	r := newSetupResult(false)
	r.Phase = "preflight"
	a := newInitAction("mcp:claudecode", "mcp", "claudecode", filepath.Join(t.TempDir(), ".mcp.json"))
	r.addError(&a, initErrorCode(parseErr, "preflight_failed"))
	r.Actions = append(r.Actions, a)
	r.finalize()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(secret)) || r.Status != "failed" || r.Complete || r.Actions == nil || r.Errors == nil {
		t.Fatalf("unexpected result: %s", data)
	}
	if !bytes.Contains(data, []byte(`"runtime_verified":false`)) {
		t.Fatalf("missing runtime flag: %s", data)
	}
	var human bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&human)
	if err := renderSetupResult(command, &r, true, false); err == nil {
		t.Fatal("quiet failed preflight returned success")
	}
	if bytes.Contains(human.Bytes(), []byte(secret)) || !bytes.Contains(human.Bytes(), []byte("invalid_document")) ||
		bytes.Contains(human.Bytes(), []byte("Prepared store")) {
		t.Fatalf("unsafe human result: %s", human.String())
	}
}

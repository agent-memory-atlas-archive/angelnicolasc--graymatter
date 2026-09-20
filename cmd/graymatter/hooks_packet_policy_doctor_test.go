package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHookPacketConfigurationDiagnostics(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "graymatter")
	settings := func(groups []any) string {
		data, err := json.Marshal(map[string]any{"hooks": map[string]any{hooksEventUserPrompt: groups}})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	foreign := map[string]any{"hooks": []any{map[string]any{
		"type": "command", "command": "other-tool", "args": []any{"hooks", "run", "user-prompt", "--packet-policy", "other-policy"},
	}}}
	for _, tc := range []struct {
		name, data, status, detail string
	}{
		{"missing file", "", "info", "not configured (settings file absent)"},
		{"no hooks", `{}`, "info", "not configured (no managed UserPromptSubmit hook)"},
		{"foreign only", settings([]any{foreign}), "info", "not configured (no managed UserPromptSubmit hook)"},
		{"implicit native", settings(hookGroupsForPolicy(exe, hooksEventUserPrompt, scopeProject, "")), "info", "native (product default; no explicit policy)"},
		{"explicit native", settings(hookGroupsForPolicy(exe, hooksEventUserPrompt, scopeProject, "native")), "info", "native (explicit installed choice)"},
		{"lexical and foreign", settings(append(hookGroupsForPolicy(exe, hooksEventUserPrompt, scopeProject, "lexical"), foreign)), "info", "lexical (explicit installed choice); experimental"},
		{"conflicting policies", settings(append(hookGroupsForPolicy(exe, hooksEventUserPrompt, scopeProject, "native"), hookGroupsForPolicy(exe, hooksEventUserPrompt, scopeProject, "lexical")...)), "fail", "conflicting installed packet policies"},
		{"invalid policy", settings(hookGroupsForPolicy(exe, hooksEventUserPrompt, scopeProject, "unknown")), "fail", "invalid or duplicate installed packet policy"},
		{"invalid JSON", `{broken`, "fail", "settings must be a valid JSON object"},
		{"null root", `null`, "fail", "settings must be a JSON object"},
		{"wrong hooks type", `{"hooks":[]}`, "fail", "hooks must be an object"},
		{"wrong prompt type", `{"hooks":{"UserPromptSubmit":{}}}`, "fail", "UserPromptSubmit hooks must be a list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			if tc.data != "" {
				if err := os.WriteFile(path, []byte(tc.data), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got := readHookPacketConfigurationCheck(path, exe, scopeProject)
			if got.Status != tc.status || !strings.Contains(got.Detail, tc.detail) || !strings.Contains(got.Detail, "project settings ("+path+")") {
				t.Fatalf("check=%+v; want %s, %q and source", got, tc.status, tc.detail)
			}
			if tc.status == "info" && !strings.HasPrefix(tc.detail, "lexical ") && !strings.Contains(got.Hint, "--scope project --packet-policy lexical") {
				t.Fatalf("missing scoped discovery hint: %+v", got)
			}
			data, err := os.ReadFile(path)
			if tc.data == "" {
				if !os.IsNotExist(err) {
					t.Fatalf("diagnosis created settings: %v", err)
				}
			} else if err != nil || string(data) != tc.data {
				t.Fatalf("diagnosis changed settings: %q, %v", data, err)
			}
		})
	}

	t.Run("read failure is not absence", func(t *testing.T) {
		got := readHookPacketConfigurationCheck(t.TempDir(), exe, scopeGlobal)
		if got.Status != "fail" || !strings.Contains(got.Detail, "cannot read settings") {
			t.Fatalf("check=%+v", got)
		}
	})
}

func TestDoctorPacketPoliciesMatchHooksDoctorWithoutStoreAccess(t *testing.T) {
	storeDir := withHooksEnv(t)
	t.Chdir(t.TempDir())
	previousHome := testHomeOverride
	testHomeOverride = t.TempDir()
	t.Cleanup(func() { testHomeOverride = previousHome })
	exe, err := resolveOwnBinary()
	if err != nil {
		t.Fatal(err)
	}
	scopes := []hookScope{scopeProject, scopeGlobal}
	policies := []string{"native", "lexical"}
	paths := make([]string, len(scopes))
	before := make([]string, len(scopes))
	for i, scope := range scopes {
		paths[i], err = claudeSettingsPath(scope)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := upsertHookSettingsPolicy(paths[i], exe, true, scope, &policies[i]); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(paths[i])
		if err != nil {
			t.Fatal(err)
		}
		before[i] = string(data)
	}
	checks := checkHookPacketPolicies()
	if len(checks) != len(scopes) {
		t.Fatalf("checks=%+v", checks)
	}
	for i, scope := range scopes {
		if checks[i].Status != "info" || !strings.Contains(checks[i].Detail, policies[i]+" (explicit installed choice)") || !strings.Contains(checks[i].Detail, string(scope)+" settings ("+paths[i]+")") {
			t.Fatalf("scope %s: %+v", scope, checks[i])
		}
		matched := 0
		for _, hookCheck := range runHooksDoctorChecks(paths[i], exe, scope) {
			if hookCheck.Name == "packet policy" {
				matched++
				if checkResult(hookCheck) != checks[i] {
					t.Fatalf("doctors disagree: general=%+v hooks=%+v", checks[i], hookCheck)
				}
			}
		}
		if matched != 1 {
			t.Fatalf("scope %s: %d packet checks", scope, matched)
		}
		data, err := os.ReadFile(paths[i])
		if err != nil || string(data) != before[i] {
			t.Fatalf("settings changed for %s: %v", scope, err)
		}
	}
	entries, err := os.ReadDir(storeDir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("diagnosis created store state: %v %v", entries, err)
	}
}

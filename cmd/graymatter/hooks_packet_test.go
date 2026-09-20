package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestHookPacketNamespaceFallback(t *testing.T) {
	for _, tc := range []struct {
		name        string
		pool        []string
		poolErr     error
		fallbackErr error
	}{
		{"recall error", nil, errors.New("unavailable"), nil},
		{"duplicates", []string{"duplicate", "duplicate"}, nil, nil},
		{"graph overflow", make([]string, 33), nil, nil},
		{"invalid UTF8", []string{"\xff"}, nil, nil},
		{"fallback error", nil, errors.New("unavailable"), errors.New("unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls []int
			facts, receipt, err := hookRecallLexicalNamespace(context.Background(), func(_ context.Context, k int) ([]string, error) {
				calls = append(calls, k)
				if k == 32 {
					return tc.pool, tc.poolErr
				}
				return []string{"actual native result"}, tc.fallbackErr
			}, "query", 3)
			if !reflect.DeepEqual(calls, []int{32, 3}) || err == nil {
				t.Fatalf("calls=%v err=%v", calls, err)
			}
			if tc.fallbackErr == nil && (!reflect.DeepEqual(facts, []string{"actual native result"}) || receipt.Effective != "native") {
				t.Fatalf("%v %+v", facts, receipt)
			}
			if tc.fallbackErr != nil && (len(facts) != 0 || receipt.Effective != "none") {
				t.Fatalf("%v %+v", facts, receipt)
			}
		})
	}
}

func TestHookPacketDeadlineDoesNotStartFallback(t *testing.T) {
	for _, alreadyCancelled := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		if alreadyCancelled {
			cancel()
		}
		calls := 0
		_, r, err := hookRecallLexicalNamespace(ctx, func(context.Context, int) ([]string, error) { calls++; cancel(); return []string{"fact"}, nil }, "query", 3)
		cancel()
		want := 1
		if alreadyCancelled {
			want = 0
		}
		if calls != want || err == nil || r.Effective != "none" {
			t.Fatalf("calls=%d receipt=%+v err=%v", calls, r, err)
		}
	}
}

func TestHookPacketNamespaceIsolationAndReceipt(t *testing.T) {
	store := &hookRecallStub{agentFacts: []string{"secret project", "secret project"}, sharedFacts: []string{"shared café no export"}}
	block, r, err := hookRecallLexicalBlock(context.Background(), store, "example", "café export")
	if err == nil || r.Project.Effective != "native" || r.Shared.Effective != "lexical" || !strings.Contains(block, "shared café") {
		t.Fatalf("%s %+v %v", block, r, err)
	}
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), "secret") || strings.Contains(string(encoded), "café") {
		t.Fatalf("receipt leaked text: %s", encoded)
	}
}

func TestHookPacketWholeFactOmissionIsNotFallback(t *testing.T) {
	calls := 0
	facts, r, err := hookRecallLexicalNamespace(context.Background(), func(context.Context, int) ([]string, error) { calls++; return []string{strings.Repeat("é", 500)}, nil }, "é", 3)
	if err != nil || calls != 1 || len(facts) != 0 || r.Effective != "lexical" || r.Reason == "" {
		t.Fatalf("%v %+v %v calls=%d", facts, r, err, calls)
	}
}

func TestHookPacketPolicyRoundTrip(t *testing.T) {
	for _, scope := range []hookScope{scopeProject, scopeGlobal} {
		t.Run(string(scope), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			exe := "/tools/graymatter"
			if _, err := upsertHookSettings(path, exe, true, scope); err != nil {
				t.Fatal(err)
			}
			check := func(want string) {
				t.Helper()
				root := readSettings(t, path)
				hooks := root["hooks"].(map[string]any)
				policy, err := installedHookPacketPolicy(hooks[hooksEventUserPrompt], exe)
				if err != nil || policy != want {
					t.Fatalf("policy=%q err=%v want=%q", policy, err, want)
				}
				if drift, err := hookSettingsDrift(path, exe, scope); err != nil || drift {
					t.Fatalf("drift=%v err=%v", drift, err)
				}
				for _, event := range hookEventNames() {
					if event != hooksEventUserPrompt && !hookArraysEqual(hooks[event].([]any), hookGroupsFor(exe, event, scope)) {
						t.Fatalf("changed %s", event)
					}
				}
				if scope == scopeGlobal {
					if _, guarded := globalHookGuardStatus(root); !guarded {
						t.Fatal("lost no-create")
					}
				}
			}
			check("")
			for _, policy := range []string{"lexical", "native"} {
				if _, err := upsertHookSettingsPolicy(path, exe, true, scope, &policy); err != nil {
					t.Fatal(err)
				}
				check(policy)
				before, _ := os.ReadFile(path)
				res, err := upsertHookSettings(path, exe, true, scope)
				after, _ := os.ReadFile(path)
				if err != nil || res.Changed || string(before) != string(after) {
					t.Fatalf("reinstall lost choice: %+v %v", res, err)
				}
				check(policy)
			}
			before, _ := os.ReadFile(path)
			bad := "model"
			if _, err := upsertHookSettingsPolicy(path, exe, true, scope, &bad); err == nil {
				t.Fatal("accepted invalid policy")
			}
			after, _ := os.ReadFile(path)
			if string(before) != string(after) {
				t.Fatal("invalid policy mutated settings")
			}
			if _, err := upsertHookSettings(path, exe, false, scope); err != nil {
				t.Fatal(err)
			}
			if _, found := readSettings(t, path)["hooks"]; found {
				t.Fatal("uninstall retained managed hooks")
			}
		})
	}
}

func TestHookPacketPolicyParsing(t *testing.T) {
	for _, args := range [][]string{{"--packet-policy"}, {"--packet-policy="}, {"--packet-policy", "semantic"}, {"--packet-policy", "native", "--packet-policy=lexical"}} {
		if _, err := hookPacketPolicyFromArgs(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	for _, policy := range []string{"native", "lexical"} {
		got, err := hookPacketPolicyFromArgs([]string{"--packet-policy=" + policy})
		if err != nil || got != policy {
			t.Fatalf("%q %v", got, err)
		}
	}
	groups := hookGroupsForPolicy("/bin/graymatter", hooksEventUserPrompt, scopeProject, "native")
	groups = append(groups, hookGroupsForPolicy("/bin/graymatter", hooksEventUserPrompt, scopeProject, "lexical")...)
	if check := hookPacketPolicyCheck(groups, "/bin/graymatter"); check.Status != "fail" {
		t.Fatalf("conflict %+v", check)
	}
}

func TestHookPacketPayloadCannotOverridePolicy(t *testing.T) {
	payload := readHookPayload(strings.NewReader(`{"prompt":"release","packet_policy":"lexical","max_bytes":999999,"agent_id":"foreign","dir":"foreign"}`))
	encoded, _ := json.Marshal(payload)
	if strings.Contains(string(encoded), "foreign") || strings.Contains(string(encoded), "packet_policy") || strings.Contains(string(encoded), "max_bytes") {
		t.Fatalf("accepted override: %s", encoded)
	}
}

func TestHookPacketChangedBlockIsNotSuppressed(t *testing.T) {
	withHooksEnv(t)
	old := renderMemoryBlock("project", []string{"old result"}, nil)
	newBlock := renderMemoryBlock("project", []string{"better result"}, nil)
	hookStateRecordBlock("project", "session", old)
	if !hookStateSeenBlock("project", "session", old) || hookStateSeenBlock("project", "session", newBlock) {
		t.Fatal("suppression must follow rendered content")
	}
}

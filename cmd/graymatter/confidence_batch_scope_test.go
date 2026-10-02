package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfidenceCLIBatchScopeReceiptAndHeader(t *testing.T) {
	f := newIssue81Fixture(t)
	project := filepath.Join(f.root, "batch-scope-cli")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	store := f.registerStore(filepath.Join(project, ".graymatter"))
	run := func(args ...string) issue81Output {
		return f.run(project, "", append([]string{"--dir", store, "--no-daemon"}, args...)...)
	}
	for _, args := range [][]string{
		{"remember", "a", "gateway routing project", "--confidence", "verified"},
		{"remember", "--shared", "gateway routing shared", "--confidence", "verified"},
	} {
		if out := run(args...); out.code != 0 {
			t.Fatalf("seed %v: %+v", args, out)
		}
	}
	for _, scope := range []string{"shared", "all"} {
		t.Run(scope, func(t *testing.T) {
			args := []string{"recall", "a", "--query", "gateway", "--query", "routing", "--" + scope, "--min-confidence", "verified", "--confidence-weight", "0"}
			text := run(args...)
			if text.code != 0 || !strings.Contains(text.stdout, "# Memory context ["+scope+"]") || !strings.Contains(text.stdout, "gateway routing shared") {
				t.Fatalf("batch text identifies the wrong scope: %+v", text)
			}
			if hasProject := strings.Contains(text.stdout, "gateway routing project"); hasProject != (scope == "all") {
				t.Fatalf("batch text crossed scope: %+v", text)
			}
			out := run(append(args, "--json")...)
			if out.code != 0 {
				t.Fatalf("batch JSON failed: %+v", out)
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(out.stdout), &payload); err != nil {
				t.Fatal(err)
			}
			if payload["scope"] != scope || payload["agent_id"] != "a" || payload["retrieval"] == nil {
				t.Fatalf("batch receipt identifies the wrong scope or changed legacy identity: %#v", payload)
			}
			wantCount := float64(1)
			if scope == "all" {
				wantCount = 2
			}
			if payload["count"] != wantCount {
				t.Fatalf("batch receipt lost namespace results: %#v", payload)
			}
		})
	}
}

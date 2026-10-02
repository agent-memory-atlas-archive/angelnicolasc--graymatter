package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/usage"
)

func executeUsage(t *testing.T, dir, input string, args ...string) (string, error) {
	t.Helper()
	cmd := usageCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader(input))
	cmd.SetArgs(append([]string{"--state-dir", dir}, args...))
	e := cmd.Execute()
	return out.String(), e
}

func TestUsageCLIImportShowAndStatusline(t *testing.T) {
	dir := t.TempDir()
	raw := `{"session_id":"synthetic-session","context_window":{"context_window_size":1000,"used_percentage":10,"current_usage":{"input_tokens":100}},"rate_limits":{"five_hour":{"used_percentage":20,"resets_at":2000000000}}}`
	if _, e := executeUsage(t, dir, raw, "statusline", "--account", "synthetic"); e != nil {
		t.Fatal(e)
	}
	text, e := executeUsage(t, dir, "", "show")
	if e != nil || !strings.Contains(text, "20.0%") || !strings.Contains(text, "context anthropic synthetic") {
		t.Fatalf("%s %v", text, e)
	}
	encoded, e := executeUsage(t, dir, "", "show", "--json")
	var s usage.Snapshot
	if e != nil || json.Unmarshal([]byte(encoded), &s) != nil || len(s.Quotas) != 3 {
		t.Fatalf("invalid JSON output: %s %v", encoded, e)
	}
	if _, e = executeUsage(t, dir, encoded, "import"); e != nil {
		t.Fatal(e)
	}
}

func TestUsageCLIRefreshErrorStillEmitsCachedJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GRAYMATTER_TEST_UNSET_KEY", "")
	if _, e := executeUsage(t, dir, "", "connect", "test", "--kind", "openai-costs", "--account", "synthetic", "--key-env", "GRAYMATTER_TEST_UNSET_KEY"); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	if e := usage.Import(context.Background(), dir, usage.Snapshot{Version: 1, Costs: []usage.CostObservation{{Provider: "openai", AccountID: "synthetic", Amount: "1", Currency: "USD", Kind: "reported", Source: "synthetic", Scope: "organization", StartAt: now.Add(-time.Hour), EndAt: now, ObservedAt: now}}}); e != nil {
		t.Fatal(e)
	}
	text, e := executeUsage(t, dir, "", "show", "--refresh", "--json")
	if e == nil {
		t.Fatal("refresh failure reported success")
	}
	var s usage.Snapshot
	if json.Unmarshal([]byte(text), &s) != nil || len(s.Costs) != 1 || s.Connections[0].State != "error" {
		t.Fatalf("cached JSON lost: %s", text)
	}
}

func TestUsageCLIConnectThenShowDoesNotCreateDatabase(t *testing.T) {
	dir := t.TempDir()
	if _, err := executeUsage(t, dir, "", "connect", "synthetic", "--kind", "openai-costs", "--account", "synthetic", "--key-env", "GRAYMATTER_SYNTHETIC_UNUSED", "--disabled"); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"show", "--json"}, {"show", "--refresh", "--json"}} {
		encoded, err := executeUsage(t, dir, "", args...)
		var s usage.Snapshot
		if err != nil || json.Unmarshal([]byte(encoded), &s) != nil || len(s.Connections) != 1 || s.Connections[0].State != "disabled" {
			t.Fatalf("config-only show: %s %v", encoded, err)
		}
		if _, err := os.Stat(filepath.Join(dir, "usage", "usage.db")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("show created a database: %v", err)
		}
	}
}

type failingUsageWriter struct{}

func (failingUsageWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic output failure")
}
func TestUsageCLIWriterErrorsPropagate(t *testing.T) {
	cmd := usageCmd()
	cmd.SetArgs([]string{"--state-dir", t.TempDir(), "show"})
	cmd.SetOut(failingUsageWriter{})
	cmd.SetErr(io.Discard)
	if e := cmd.Execute(); e == nil || !strings.Contains(e.Error(), "synthetic output failure") {
		t.Fatalf("writer failure ignored: %v", e)
	}
}

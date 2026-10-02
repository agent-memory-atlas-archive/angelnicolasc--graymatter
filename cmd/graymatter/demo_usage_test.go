package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	graymatter "github.com/angelnicolasc/graymatter"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/usage"
)

func TestDemoProviderConfigurationAlwaysOffline(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "fixture-not-a-secret")
	t.Setenv("OPENAI_API_KEY", "fixture-not-a-secret")
	t.Setenv("VOYAGE_API_KEY", "fixture-not-a-secret")
	t.Setenv("GRAYMATTER_KG_LLM_EXTRACT", "1")
	cfg := demoConfig(t.TempDir())
	if cfg.EmbeddingMode != graymatter.EmbeddingKeyword || cfg.ConsolidateLLM != "" || cfg.AnthropicAPIKey != "" || cfg.OpenAIAPIKey != "" || cfg.VoyageAPIKey != "" || cfg.AsyncConsolidate {
		t.Fatal("demo inherited a remote provider")
	}
	st, err := openDemoStore(context.Background(), cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if !st.Demo() {
		t.Fatal("sample data cannot be distinguished from a real store")
	}
	if err := st.Remember(context.Background(), "demo", "PostgreSQL stores project records"); err != nil {
		t.Fatal(err)
	}
}

func TestDemoUsageIsLabelledAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	for range 2 {
		if err := seedDemoUsage(context.Background(), dir); err != nil {
			t.Fatal(err)
		}
	}
	s, err := usage.Load(context.Background(), dir, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Quotas) != 1 || len(s.Costs) != 1 || len(s.Contexts) != 1 {
		t.Fatalf("reseed duplicated sample observations: %+v", s)
	}
	if s.Quotas[0].Source != "demo / synthetic" || s.Costs[0].Source != "demo / synthetic" || s.Contexts[0].Source != "demo / synthetic" {
		t.Fatal("sample observations are not labelled")
	}
}

func TestDemoFreshAcceptsItsUsageLedger(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo")
	if err := seedDemoUsage(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if err := removeDemoDir(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("demo directory survived reset: %v", err)
	}
}

func TestDemoScriptPreservesOfflineEntryPointAndQuotes(t *testing.T) {
	cmd := demoCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := cmd.Flags().Set("no-tui", "true"); err != nil {
		t.Fatal(err)
	}
	if err := printDemoScript(cmd, "/tmp/user's $data"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "graymatter demo --dir '/tmp/user'\"'\"'s $data' --no-tui\n") {
		t.Fatalf("script did not quote path or preserve offline entrypoint: %s", output.String())
	}
	for _, line := range strings.Split(output.String(), "\n") {
		if strings.HasPrefix(line, "graymatter ") && !strings.HasPrefix(line, "graymatter demo ") {
			t.Fatal("script invokes a provider-configured command outside the demo")
		}
	}
}

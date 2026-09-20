package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
)

// TestHookLatencyBudgets runs the real binary against the machine-relative
// contract: the median user-prompt and session-end deltas from pre-compact
// must stay within recallDeltaBudget and sessionEndDeltaBudget (200 ms each),
// and normalized scaling must stay within recallScalingMaxNormalized (2.5x).
// Pre-compact is the per-run baseline and has no absolute gate.
//
// CI runs this timing measurement report-only with continue-on-error; it is
// deliberately excluded from the blocking benchmark-package tests because
// shared-runner timings are noisy.
func TestHookLatencyBudgets(t *testing.T) {
	if testing.Short() {
		t.Skip("hook latency gate needs process spawns; skipped in -short")
	}

	var buf bytes.Buffer
	start := time.Now()
	if err := run(&buf); err != nil {
		t.Fatalf("hook latency gate failed after %s:\n%s\n%v", time.Since(start).Round(time.Second), buf.String(), err)
	}
	t.Logf("gate passed in %s:\n%s", time.Since(start).Round(time.Second), buf.String())

	// The report must name every gated event — a gate that silently stops
	// measuring one of them is worse than a failing one.
	out := buf.String()
	if !strings.Contains(out, "Embedder: keyword (no LLM, no network, no API key)") {
		t.Error("report missing the explicit keyword embedder")
	}
	for _, event := range []string{"user-prompt", "pre-compact", "session-end"} {
		if !strings.Contains(out, event) {
			t.Errorf("report missing the %s row", event)
		}
	}
	for _, policy := range []string{"native", "lexical"} {
		for _, size := range []int{smallFacts, seedFacts} {
			if !strings.Contains(out, fmt.Sprintf("policy=%s facts=%d", policy, size)) {
				t.Errorf("report missing %s/%d cell", policy, size)
			}
		}
		if !strings.Contains(out, policy+" full hook (median)") {
			t.Errorf("report missing %s hook scaling", policy)
		}
	}
}

func TestBenchmarkConfigIgnoresProvidersAndRetrievalSwitches(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "VOYAGE_API_KEY"} {
		t.Setenv(key, "benchmark-test-only")
	}
	t.Setenv("GRAYMATTER_STEM_KEYWORDS", "0")
	t.Setenv("GRAYMATTER_CANDIDATE_RETRIEVAL", "0")
	t.Setenv("GRAYMATTER_USAGE_ALIAS", "1")
	t.Setenv("GRAYMATTER_USAGE_ALIAS_AFFINITY", "4")
	cfg := benchmarkConfig("benchmark-store")
	if cfg.EmbeddingMode != graymatter.EmbeddingKeyword || cfg.ConsolidateLLM != "" ||
		cfg.AnthropicAPIKey != "" || cfg.OpenAIAPIKey != "" || cfg.VoyageAPIKey != "" ||
		cfg.AsyncConsolidate || cfg.VectorReconcileInterval != 0 || !cfg.StemKeywords ||
		!cfg.CandidateRetrieval || cfg.UsageAliasLearning || cfg.UsageAliasAffinityMin != 0 {
		t.Fatal("in-process benchmark inherited a provider or retrieval override")
	}
	// os/exec resolves duplicate environment keys using their last value.
	values := make(map[string]string)
	for _, entry := range benchmarkEnv([]string{"PATH=keep-path", "OPENAI_API_KEY=benchmark-test-only", "GRAYMATTER_OLLAMA_URL=http://localhost:11434"}) {
		key, value, _ := strings.Cut(entry, "=")
		values[key] = value
	}
	if values["PATH"] != "keep-path" || values["OPENAI_API_KEY"] != "" ||
		values["ANTHROPIC_API_KEY"] != "" || values["VOYAGE_API_KEY"] != "" ||
		values["GRAYMATTER_OLLAMA_URL"] != "disabled://hook-latency-benchmark" ||
		values["GRAYMATTER_STEM_KEYWORDS"] != "1" || values["GRAYMATTER_CANDIDATE_RETRIEVAL"] != "1" ||
		values["GRAYMATTER_USAGE_ALIAS"] != "0" || values["GRAYMATTER_USAGE_ALIAS_AFFINITY"] != "0" {
		t.Fatal("child benchmark environment did not isolate providers and retrieval switches")
	}
}

func TestHookPayloadMatchesEventAndSession(t *testing.T) {
	for event, want := range map[string]string{"user-prompt": "UserPromptSubmit", "pre-compact": "PreCompact", "session-end": "SessionEnd"} {
		var payload map[string]string
		if err := json.Unmarshal([]byte(hookPayload(`C:\a space\project`, `query "quoted"`, event, "sample-3")), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["hook_event_name"] != want || payload["session_id"] != "sample-3" || payload["cwd"] != `C:\a space\project` || payload["prompt"] != `query "quoted"` {
			t.Fatalf("incorrect %s payload", event)
		}
	}
}

func TestLexicalReceiptRejectsFallbackAndStaleSamples(t *testing.T) {
	ms := 20.0
	entry := hookEntry{Event: "user-prompt", Outcome: "ok", Detail: "injected", Session: "measured", Ms: &ms}
	output := fmt.Sprintf("[GrayMatter hook recall ran for agent_id=%q.]\n## Memory\nFact", benchAgent)
	if err := validateHookEntry(entry, "user-prompt", "native", "measured", output); err != nil {
		t.Fatal(err)
	}
	if err := validateHookEntry(entry, "user-prompt", "lexical", "measured", output); err == nil {
		t.Fatal("missing lexical receipt accepted")
	}
	packet := &packetEntry{Policy: "lexical", Unit: "utf8_bytes", MaxBytes: 832}
	packet.Project.Effective, packet.Shared.Effective = "lexical", "lexical"
	packet.Project.Candidates, packet.Project.Selected, packet.Project.Bytes = 32, 3, 250
	entry.Packet = packet
	if err := validateHookEntry(entry, "user-prompt", "lexical", "measured", output); err != nil {
		t.Fatal(err)
	}
	if err := validateHookEntry(entry, "user-prompt", "lexical", "next-sample", output); err == nil {
		t.Fatal("stale session receipt accepted")
	}
	packet.Project.Effective = "native"
	if err := validateHookEntry(entry, "user-prompt", "lexical", "measured", output); err == nil {
		t.Fatal("native fallback counted as lexical")
	}
}

func TestPacketReceiptBoundToFinalSession(t *testing.T) {
	dir := t.TempDir()
	packet := `{"policy":"lexical","unit":"utf8_bytes","max_bytes":832,"project":{"effective":"lexical"},"shared":{"effective":"lexical"}}`
	ms := 3.0
	lines := []hookEntry{
		{Event: "user-prompt", Session: "previous", Outcome: "packet", Detail: packet},
		{Event: "user-prompt", Session: "current", Outcome: "ok", Detail: "injected", Ms: &ms},
	}
	var buf bytes.Buffer
	for _, line := range lines {
		if err := json.NewEncoder(&buf).Encode(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "hooks.log"), buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	entry, err := lastHookEntry(dir)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Packet != nil || entry.Session != "current" {
		t.Fatal("packet receipt from another session was attached to final event")
	}
}

func TestMedianGateAndAdditionalP99CheckStaySeparate(t *testing.T) {
	results := make(map[string][]sample)
	for _, event := range []string{"user-prompt", "pre-compact", "session-end"} {
		for i := 0; i < measuredRuns; i++ {
			results[event] = append(results[event], sample{event: event, internal: 10 * time.Millisecond, wall: 20 * time.Millisecond})
		}
	}
	results["user-prompt"][0].internal = 211 * time.Millisecond
	var buf bytes.Buffer
	fails, extra := reportPolicy(&buf, results)
	if fails != 0 || extra != 1 {
		t.Fatalf("outlier changed median gate: failures=%d p99=%d", fails, extra)
	}
	for i := range results["user-prompt"] {
		results["user-prompt"][i].internal = 211 * time.Millisecond
	}
	fails, extra = reportPolicy(&buf, results)
	if fails != 1 || extra != 1 {
		t.Fatalf("median breach not detected: failures=%d p99=%d", fails, extra)
	}
}

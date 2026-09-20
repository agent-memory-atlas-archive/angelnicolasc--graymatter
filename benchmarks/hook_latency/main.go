// hook_latency measures the Claude Code hooks' hot path against what the code
// actually controls — never against shared-CI hardware. CI runs it report-only.
//
// Absolute wall-clock budgets are a hardware lottery: the same tree measured
// p99 user-prompt 121 ms on a dev machine, 170 ms on macOS runners and
// 284-369 ms on Windows runners, with the spawn+connect baseline alone
// (no recall involved) reaching 192-248 ms on Windows — an absolute 200 ms
// "checkpoint budget" failed there without a single line of Go being wrong.
// Gating absolutes on shared runners gates the runner queue, not the code.
//
// What the benchmark checks instead is machine-relative:
//
//  1. recall delta     user-prompt median − pre-compact median ≤ 200 ms
//     (pre-compact measures this machine's connect+checkpoint cost;
//     the delta isolates the recall's marginal cost — the part the
//     hot-path optimizations own, and where a reintroduced double
//     tokenize or full decode shows up immediately)
//  2. session-end delta session-end median − pre-compact median ≤ 200 ms
//     (the detached consolidation spawn must add almost nothing)
//  3. scaling          (Recall(10k)/Recall(500)) / (10000/500)
//     ≤ recallScalingMaxNormalized (1.0x is linear; catches algorithmic
//     blowups — accidental O(n²) passes, full re-decodes)
//
// Absolute numbers are still measured and printed: they are reference data
// for humans, and the published reference-hardware figure (user-prompt p99
// 121 ms on the dev machine that set the budgets) stays in the README beside
// the checks this benchmark reports. CI does not block merges on this result.
//
// Both policies run through the real hook at both store sizes. Queries overlap
// the corpus and each prompt has its own session id, so identical-block
// throttling cannot suppress a sample. Hook-internal and process wall times
// are reported separately; only wall time includes process creation.
//
// Usage:
//
//	go test ./benchmarks/hook_latency/   # CI report-only measurement
//	go run  ./benchmarks/hook_latency    # human-readable report
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	graymatter "github.com/angelnicolasc/graymatter"
)

const (
	seedFacts     = 10000
	smallFacts    = 500
	warmupSamples = 3
	measuredRuns  = 12

	// The machine-relative budgets: deltas are measured against the
	// pre-compact baseline on the same machine in the same run. 200 ms
	// leaves ~1.5-2.5x headroom over every runner measured so far
	// (deltas: ~90 ms dev, ~136 ms macOS, ~92 ms Windows) while a
	// reintroduced double-tokenize (+40 ms) or a per-fact write txn
	// (+500 ms) breach it decisively.
	recallDeltaBudget     = 200 * time.Millisecond
	sessionEndDeltaBudget = 200 * time.Millisecond
	// Scaling is normalized by the size ratio (Recall(10k)/Recall(500)) /
	// (10000/500), so 1.0x is exactly linear. Measured on the reference
	// machine: 1.17x (cache and GC make ten-thousand-item work slightly
	// super-linear). The gate sits at 2.5x normalized — far above anything
	// cache noise produces, far below the 20x normalized that an accidental
	// quadratic pass would show.
	recallScalingMaxNormalized = 2.5

	// benchAgent is the basename of the working directory the hook runs
	// from, so deriveAgentID resolves to exactly the seeded agent.
	benchAgent = "hookbench"
)

type sample struct {
	event    string
	internal time.Duration // the hook's own clock (hooks.log "ms")
	wall     time.Duration // process start to exit
}

func main() {
	if err := run(os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "hook_latency: %v\n", err)
		os.Exit(1)
	}
}

// run is the whole benchmark; main and the CI test both drive it.
func run(stdout io.Writer) error {
	root, err := os.MkdirTemp("", "graymatter-hook-latency-")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(root) }()
	workDir := filepath.Join(root, benchAgent)
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return fmt.Errorf("work dir: %w", err)
	}
	binary, cleanup, err := buildBinary(root)
	if err != nil {
		return err
	}
	defer cleanup()

	fmt.Fprintln(stdout, "Embedder: keyword (no LLM, no network, no API key)")
	fmt.Fprintf(stdout, "policies: native, lexical; stores: %d and %d project facts, empty shared namespace\n", smallFacts, seedFacts)
	fmt.Fprintf(stdout, "per cell: one daemon-cold prompt, %d warm-up + %d measured process runs per event\n", warmupSamples, measuredRuns)
	fmt.Fprintln(stdout, "cold means no running daemon, not an empty OS disk cache; 12-sample p99 is the observed maximum")
	fmt.Fprintln(stdout, "existing median gates and additional p99 checks are reported separately")

	fails, p99Breaches := 0, 0
	byPolicy := make(map[string]map[int]map[string][]sample)
	var nativeScaling float64
	for _, policy := range []string{"native", "lexical"} {
		byPolicy[policy] = make(map[int]map[string][]sample)
		for _, facts := range []int{smallFacts, seedFacts} {
			storeDir := filepath.Join(root, fmt.Sprintf("%s-%d", policy, facts))
			if err := seedStoreN(storeDir, facts); err != nil {
				return fmt.Errorf("seed %s/%d: %w", policy, facts, err)
			}
			// Retain the original in-process native Recall scaling measurement.
			// It owns the store before any daemon has been started.
			if policy == "native" && facts == seedFacts {
				nativeScaling, err = measureRecallScaling(storeDir)
				if err != nil {
					return fmt.Errorf("native Recall scaling: %w", err)
				}
			}
			cold, results, err := measurePolicy(binary, workDir, storeDir, policy)
			if err != nil {
				return fmt.Errorf("%s/%d: %w", policy, facts, err)
			}
			byPolicy[policy][facts] = results
			fmt.Fprintf(stdout, "\npolicy=%s facts=%d\n", policy, facts)
			fmt.Fprintf(stdout, "  daemon-cold user-prompt internal %7.1fms · wall %7.1fms (one observation; no cold gate)\n", ms(cold.internal), ms(cold.wall))
			cellFails, cellP99Breaches := reportPolicy(stdout, results)
			fails += cellFails
			p99Breaches += cellP99Breaches
		}
	}
	fmt.Fprintln(stdout)
	if !reportScaling(stdout, "native Recall in-process", nativeScaling) {
		fails++
	}
	for _, policy := range []string{"native", "lexical"} {
		smallMedian := percentile(internalDurations(byPolicy[policy][smallFacts]["user-prompt"]), 0.5)
		bigMedian := percentile(internalDurations(byPolicy[policy][seedFacts]["user-prompt"]), 0.5)
		if smallMedian <= 0 || bigMedian <= 0 {
			return fmt.Errorf("%s hook scaling has a nonpositive duration", policy)
		}
		if !reportScaling(stdout, policy+" full hook (median)", float64(bigMedian)/float64(smallMedian)) {
			fails++
		}
	}
	for _, facts := range []int{smallFacts, seedFacts} {
		native, lexical := byPolicy["native"][facts]["user-prompt"], byPolicy["lexical"][facts]["user-prompt"]
		fmt.Fprintf(stdout, "lexical minus native, %d facts: internal median %+.1fms · wall median %+.1fms (whole policy path, not selection alone)\n",
			facts, ms(percentile(internalDurations(lexical), 0.5)-percentile(internalDurations(native), 0.5)),
			ms(percentile(wallDurations(lexical), 0.5)-percentile(wallDurations(native), 0.5)))
	}
	fmt.Fprintf(stdout, "additional p99 checks: %d breach(es); observational with %d samples per cell\n", p99Breaches, measuredRuns)
	if fails > 0 {
		return fmt.Errorf("%d hook gate(s) breached (median deltas and normalized scaling; p99 checks reported separately)", fails)
	}
	fmt.Fprintln(stdout, "all hook gates hold")
	return nil
}

// measurePolicy owns one fresh daemon/store cell and stops it before returning.
// Every prompt uses a distinct session id, so identical-block throttling cannot
// turn a recall measurement into an empty-output measurement.
func measurePolicy(binary, workDir, storeDir, policy string) (sample, map[string][]sample, error) {
	defer stopDaemon(binary, storeDir)
	runHook := func(event, prompt, session string) (sample, error) {
		start := time.Now()
		out, err := execHook(binary, workDir, storeDir, event, policy, hookPayload(workDir, prompt, event, session))
		wall := time.Since(start)
		if err != nil {
			return sample{}, fmt.Errorf("%s: %w: %s", event, err, out)
		}
		entry, err := lastHookEntry(storeDir)
		if err != nil {
			return sample{}, fmt.Errorf("%s: hooks.log: %w", event, err)
		}
		if err := validateHookEntry(entry, event, policy, session, out); err != nil {
			return sample{}, err
		}
		return sample{event: event, internal: time.Duration(*entry.Ms * float64(time.Millisecond)), wall: wall}, nil
	}
	cold, err := runHook("user-prompt", "runbook 96 subsystem review cycle", "bench-cold")
	if err != nil {
		return sample{}, nil, fmt.Errorf("daemon-cold: %w", err)
	}
	for i := 0; i < warmupSamples; i++ {
		if _, err := runHook("user-prompt", fmt.Sprintf("runbook %d subsystem review cycle", i), fmt.Sprintf("bench-warm-%d", i)); err != nil {
			return sample{}, nil, fmt.Errorf("warm-up: %w", err)
		}
	}
	results := make(map[string][]sample)
	for i := 0; i < measuredRuns; i++ {
		// Alternate order to distribute drift between the recall and baseline.
		events := []string{"user-prompt", "pre-compact"}
		if i%2 != 0 {
			events[0], events[1] = events[1], events[0]
		}
		for _, event := range events {
			prompt := ""
			if event == "user-prompt" {
				prompt = fmt.Sprintf("runbook %d subsystem review cycle", i)
			}
			s, err := runHook(event, prompt, fmt.Sprintf("bench-measured-%d", i))
			if err != nil {
				return sample{}, nil, err
			}
			results[event] = append(results[event], s)
		}
	}
	// Session-end starts detached consolidation. Measure it after prompt and
	// baseline samples so that background work cannot distort their comparison.
	for i := 0; i < measuredRuns; i++ {
		s, err := runHook("session-end", "", fmt.Sprintf("bench-end-%d", i))
		if err != nil {
			return sample{}, nil, err
		}
		results["session-end"] = append(results["session-end"], s)
	}
	return cold, results, nil
}

func reportPolicy(stdout io.Writer, results map[string][]sample) (fails, p99Breaches int) {
	baseline := internalDurations(results["pre-compact"])
	baseMedian, baseP99 := percentile(baseline, 0.5), percentile(baseline, 0.99)
	for _, event := range []string{"user-prompt", "pre-compact", "session-end"} {
		ss := results[event]
		internal, wall := internalDurations(ss), wallDurations(ss)
		median, p99 := percentile(internal, 0.5), percentile(internal, 0.99)
		note := ""
		budget := time.Duration(0)
		if event == "user-prompt" {
			budget = recallDeltaBudget
		} else if event == "session-end" {
			budget = sessionEndDeltaBudget
		}
		if budget > 0 {
			medianStatus, p99Status := "ok", "ok"
			if median-baseMedian > budget {
				medianStatus = "FAIL"
				fails++
			}
			if p99-baseP99 > budget {
				p99Status = "BREACH"
				p99Breaches++
			}
			note = fmt.Sprintf(" · delta(med) %+.1fms ≤ %v %s · extra delta(p99) %+.1fms %s", ms(median-baseMedian), budget, medianStatus, ms(p99-baseP99), p99Status)
		}
		fmt.Fprintf(stdout, "  %-12s internal med %7.1fms · p99 %7.1fms · wall med %7.1fms · wall p99/max %7.1fms%s\n", event, ms(median), ms(p99), ms(percentile(wall, 0.5)), ms(maxOf(wall)), note)
	}
	return fails, p99Breaches
}

func reportScaling(stdout io.Writer, label string, ratio float64) bool {
	normalized := ratio / (float64(seedFacts) / float64(smallFacts))
	status := "ok"
	if normalized > recallScalingMaxNormalized {
		status = "FAIL"
	}
	fmt.Fprintf(stdout, "scaling %-27s 10k/500 = %.2fx raw · %.2fx of linear (max %.1fx) · %s\n", label, ratio, normalized, recallScalingMaxNormalized, status)
	return status == "ok"
}

func stopDaemon(binary, storeDir string) {
	cmd := exec.Command(binary, "--dir", storeDir, "daemon", "stop")
	cmd.Env = benchmarkEnv(cmd.Environ())
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	_ = cmd.Run()
	// The daemon releases its store and executable asynchronously after stop.
	time.Sleep(500 * time.Millisecond)
}

// measureRecallScaling times in-process Recall over two store sizes and
// returns the ratio. Both stores are opened and closed inside this function
// while the calling process is still the store's only owner.
func measureRecallScaling(dir string) (float64, error) {
	open := func(dataDir string, n int) (*graymatter.Memory, error) {
		cfg := benchmarkConfig(dataDir)
		mem, err := graymatter.NewWithConfig(cfg)
		if err != nil {
			return nil, err
		}
		ctx := context.Background()
		for i := 0; i < n; i++ {
			if err := mem.Remember(ctx, benchAgent, fmt.Sprintf("scaling fact %d: the %s subsystem follows runbook %d", i, topicsFor(i), i%97)); err != nil {
				_ = mem.Close()
				return nil, err
			}
		}
		return mem, nil
	}
	bestOf := func(mem *graymatter.Memory, runs int) (time.Duration, error) {
		ctx := context.Background()
		var best time.Duration
		for i := 0; i < runs; i++ {
			start := time.Now()
			if _, err := mem.Recall(ctx, benchAgent, fmt.Sprintf("runbook %d subsystem review cycle %d", i, i)); err != nil {
				return 0, err
			}
			if d := time.Since(start); best == 0 || d < best {
				best = d
			}
		}
		return best, nil
	}

	smallDir, err := os.MkdirTemp(filepath.Dir(dir), "scaling-small-")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.RemoveAll(smallDir) }()

	small, err := open(smallDir, smallFacts)
	if err != nil {
		return 0, err
	}
	smallBest, err := bestOf(small, 3)
	_ = small.Close()
	if err != nil {
		return 0, err
	}

	big, err := open(dir, 0) // dir is already seeded with seedFacts
	if err != nil {
		return 0, err
	}
	bigBest, err := bestOf(big, 3)
	_ = big.Close()
	if err != nil {
		return 0, err
	}

	if smallBest <= 0 {
		return 0, fmt.Errorf("small recall measured %v", smallBest)
	}
	if bigBest <= 0 {
		return 0, fmt.Errorf("big recall measured %v", bigBest)
	}
	return float64(bigBest) / float64(smallBest), nil
}

func topicsFor(i int) string {
	topics := []string{"deploy", "database", "cache", "auth", "billing", "search", "queue", "logging", "metrics", "oncall"}
	return topics[i%len(topics)]
}

// execHook runs the hook runner as one fresh process with the benchmark store
// as its data dir, the way Claude Code invokes it (stdin JSON, output drained).
func execHook(binary, workDir, storeDir, event, policy, payload string) (string, error) {
	args := []string{"--dir", storeDir, "hooks", "run", event}
	if event == "user-prompt" {
		args = append(args, "--packet-policy", policy)
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = workDir
	cmd.Env = benchmarkEnv(cmd.Environ())
	cmd.Stdin = strings.NewReader(payload)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return out.String(), err
}

// Disable provider auto-detection without introducing a product-only benchmark
// flag. net/http rejects the unsupported Ollama URL scheme before a connection;
// empty credentials disable the remaining providers and consolidation models.
// Retrieval feature switches are pinned to the defaults used by this corpus.
func benchmarkEnv(env []string) []string {
	return append(env,
		"ANTHROPIC_API_KEY=", "OPENAI_API_KEY=", "VOYAGE_API_KEY=",
		"GRAYMATTER_OLLAMA_URL=disabled://hook-latency-benchmark",
		"GRAYMATTER_STEM_KEYWORDS=1", "GRAYMATTER_CANDIDATE_RETRIEVAL=1",
		"GRAYMATTER_USAGE_ALIAS=0", "GRAYMATTER_USAGE_ALIAS_AFFINITY=0",
	)
}

func hookPayload(workDir, prompt, event, session string) string {
	eventName := map[string]string{"user-prompt": "UserPromptSubmit", "pre-compact": "PreCompact", "session-end": "SessionEnd"}[event]
	payload, _ := json.Marshal(struct {
		SessionID string `json:"session_id"`
		CWD       string `json:"cwd"`
		Event     string `json:"hook_event_name"`
		Prompt    string `json:"prompt,omitempty"`
	}{session, workDir, eventName, prompt})
	return string(payload)
}

type hookEntry struct {
	Event   string       `json:"event"`
	Outcome string       `json:"outcome"`
	Detail  string       `json:"detail"`
	Session string       `json:"session"`
	Ms      *float64     `json:"ms"`
	Packet  *packetEntry `json:"-"`
}

type packetEntry struct {
	Policy   string `json:"policy"`
	Unit     string `json:"unit"`
	MaxBytes int    `json:"max_bytes"`
	Project  struct {
		Effective  string `json:"effective"`
		Candidates int    `json:"candidates"`
		Selected   int    `json:"selected"`
		Bytes      int    `json:"bytes"`
		Reason     string `json:"reason"`
	} `json:"project"`
	Shared struct {
		Effective string `json:"effective"`
	} `json:"shared"`
}

func validateHookEntry(entry hookEntry, event, policy, session, output string) error {
	if entry.Event != event || entry.Session != session || entry.Outcome != "ok" || entry.Ms == nil || *entry.Ms < 0 {
		return fmt.Errorf("%s: invalid hook receipt (event=%q outcome=%q detail=%q)", event, entry.Event, entry.Outcome, entry.Detail)
	}
	if event != "user-prompt" {
		return nil
	}
	marker := fmt.Sprintf("[GrayMatter hook recall ran for agent_id=%q.]", benchAgent)
	if entry.Detail != "injected" || !strings.Contains(output, marker) {
		return fmt.Errorf("user-prompt did not inject a memory block for %s", benchAgent)
	}
	if policy == "lexical" {
		p := entry.Packet
		if p == nil || p.Policy != policy || p.Unit != "utf8_bytes" || p.MaxBytes != 832 ||
			p.Project.Effective != policy || p.Shared.Effective != policy ||
			p.Project.Candidates <= 0 || p.Project.Candidates > 32 || p.Project.Selected <= 0 || p.Project.Selected > 3 ||
			p.Project.Bytes <= 0 || p.Project.Bytes > p.MaxBytes || p.Project.Reason != "" {
			return fmt.Errorf("lexical prompt did not produce a valid lexical packet receipt (fallback is not a lexical sample)")
		}
	} else if entry.Packet != nil {
		return fmt.Errorf("native prompt unexpectedly produced a lexical packet receipt")
	}
	return nil
}

func lastHookEntry(storeDir string) (hookEntry, error) {
	f, err := os.Open(filepath.Join(storeDir, "hooks.log"))
	if err != nil {
		return hookEntry{}, err
	}
	defer func() { _ = f.Close() }()
	var entry, packetLog hookEntry
	var packet *packetEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 4096), 64*1024)
	for sc.Scan() {
		if line := bytes.TrimSpace(sc.Bytes()); len(line) != 0 {
			entry = hookEntry{}
			if err := json.Unmarshal(line, &entry); err != nil {
				return hookEntry{}, err
			}
			if entry.Outcome == "packet" {
				packet = new(packetEntry)
				if err := json.Unmarshal([]byte(entry.Detail), packet); err != nil {
					return hookEntry{}, err
				}
				packetLog = entry
			}
		}
	}
	if err := sc.Err(); err != nil {
		return hookEntry{}, err
	}
	if packet != nil && packetLog.Session == entry.Session && packetLog.Event == entry.Event {
		entry.Packet = packet
	}
	return entry, nil
}

// internalDurations extracts the hook-internal timing series.
func internalDurations(ss []sample) []time.Duration {
	out := make([]time.Duration, len(ss))
	for i, s := range ss {
		out[i] = s.internal
	}
	return out
}

// wallDurations extracts the end-to-end timing series.
func wallDurations(ss []sample) []time.Duration {
	out := make([]time.Duration, len(ss))
	for i, s := range ss {
		out[i] = s.wall
	}
	return out
}

func percentile(durs []time.Duration, p float64) time.Duration {
	sorted := make([]time.Duration, len(durs))
	copy(sorted, durs)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1)*p + 0.5)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func maxOf(durs []time.Duration) time.Duration {
	var m time.Duration
	for _, d := range durs {
		if d > m {
			m = d
		}
	}
	return m
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000.0 }

// cliModuleDir walks up from the working directory to the checkout root and
// returns the CLI module's own directory. go test runs in the package dir and
// go run in the caller's, so a fixed relative path is not dependable, and
// runtime.Caller is rewritten by -trimpath. The module file is the anchor.
func cliModuleDir() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "cmd", "graymatter", "go.mod")); err == nil {
			return filepath.Join(dir, "cmd", "graymatter"), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no cmd/graymatter/go.mod above the working directory")
		}
		dir = parent
	}
}

// buildBinary compiles the current tree; the cleanup removes the artifact.
//
// The build runs inside cmd/graymatter and names the package by its local
// path. The CLI is a separate module, so resolving it by import path only
// works while go.work is in play — and CI runs this step with GOWORK=off to
// keep the module graph off the proxy, which left the gate reporting a build
// error instead of a measurement. Building from the module's own directory
// needs neither the workspace nor a network fetch.
func buildBinary(dir string) (string, func(), error) {
	moduleDir, err := cliModuleDir()
	if err != nil {
		return "", nil, fmt.Errorf("locate CLI module: %w", err)
	}
	bin := filepath.Join(dir, "graymatter-hooklatency.bin")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = moduleDir
	out, err := build.CombinedOutput()
	if err != nil {
		return "", nil, fmt.Errorf("build binary: %v: %s", err, out)
	}
	return bin, func() { _ = os.Remove(bin) }, nil
}

// seedStoreN plants n facts through the library (one process, no daemon),
// with per-fact distinct texts so recall exercises real keyword scoring over
// a realistic corpus.
func seedStoreN(dir string, n int) error {
	cfg := benchmarkConfig(dir)
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = mem.Close() }()

	ctx := context.Background()
	topics := []string{"deploy", "database", "cache", "auth", "billing", "search", "queue", "logging", "metrics", "oncall"}
	for i := 0; i < n; i++ {
		topic := topics[i%len(topics)]
		text := fmt.Sprintf("Fact %d: the %s subsystem follows runbook %d and was last reviewed on cycle %d",
			i, topic, i%97, i%13)
		if err := mem.Remember(ctx, benchAgent, text); err != nil {
			return err
		}
	}
	return nil
}

// benchmarkConfig also clears the in-process model path: merely selecting the
// keyword embedder would leave an ambient Anthropic consolidation key active.
func benchmarkConfig(dir string) graymatter.Config {
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = dir
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.ConsolidateLLM = ""
	cfg.AnthropicAPIKey, cfg.OpenAIAPIKey, cfg.VoyageAPIKey = "", "", ""
	cfg.VectorReconcileInterval = 0
	cfg.AsyncConsolidate = false
	cfg.StemKeywords = true
	cfg.CandidateRetrieval = true
	cfg.UsageAliasLearning = false
	cfg.UsageAliasAffinityMin = 0
	return cfg
}

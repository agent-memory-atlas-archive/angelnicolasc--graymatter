package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func ptr[T any](v T) *T { return &v }
func sample(now time.Time) Snapshot {
	return Snapshot{Version: Version, GeneratedAt: now, Quotas: []QuotaSnapshot{{Provider: "openai", AccountID: "personal", Source: "test", Label: "Codex", Window: "primary", UsedPercent: ptr(20.0), ResetAt: ptr(now.Add(time.Hour)), ObservedAt: now}}, Costs: []CostObservation{{Provider: "openai", AccountID: "org", Source: "test", Scope: "organization", Kind: "reported", Amount: "1.25", Currency: "USD", StartAt: now.Add(-time.Hour), EndAt: now, ObservedAt: now}}, Events: []UsageEvent{{Provider: "google", AccountID: "api-project", Source: "telemetry", RequestID: "request-1", Model: "test-model", Operation: "generate", Time: now, Quantities: map[string]int64{"input_uncached": 100, "input_cache_read": 20, "output": 30}}}, Contexts: []ContextSnapshot{{Provider: "google", AccountID: "api-project", Source: "telemetry", SessionID: "session-1", Model: "test-model", UsedTokens: ptr(int64(120)), ObservedAt: now}}}
}

func TestLoadEmptyHasNoSideEffects(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	s, e := Load(context.Background(), dir, false)
	if e != nil || s.Version != Version || len(s.Quotas) != 0 {
		t.Fatalf("%+v %v", s, e)
	}
	if _, e = os.Stat(dir); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("read created state: %v", e)
	}
}

func TestConfigOnlyLoadsDoNotCreateDatabase(t *testing.T) {
	root := t.TempDir()
	config := Config{Version: Version, Connections: []Connection{{ID: "disabled", Kind: "openai-costs", Provider: "openai", AccountID: "synthetic", APIKeyEnv: "GRAYMATTER_SYNTHETIC_UNUSED", Enabled: false}}}
	if err := SaveConfig(root, config); err != nil {
		t.Fatal(err)
	}
	for _, refresh := range []bool{false, true} {
		s, err := Load(context.Background(), root, refresh)
		if err != nil || s.Version != Version || len(s.Connections) != 1 || s.Connections[0].State != "disabled" {
			t.Fatalf("config-only load refresh=%v: %+v %v", refresh, s, err)
		}
		if _, err := os.Stat(filepath.Join(usageDir(root), "usage.db")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("config-only load created a database: %v", err)
		}
	}
	// A pre-existing invalid file remains an error and must not be initialized
	// or presented as a complete snapshot that replaces the UI's good cache.
	path := filepath.Join(usageDir(root), "usage.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(context.Background(), root, false)
	if err == nil || s.Version != 0 {
		t.Fatalf("invalid database returned a usable snapshot: %+v %v", s, err)
	}
	info, statErr := os.Stat(path)
	if statErr != nil || info.Size() != 0 {
		t.Fatalf("read modified invalid database: %v %v", info, statErr)
	}
}

func TestImportIdempotenceAtomicityAndConcurrentWriters(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	now := time.Now().UTC()
	s := sample(now)
	for i := 0; i < 2; i++ {
		if e := Import(ctx, root, s); e != nil {
			t.Fatal(e)
		}
	}
	got, e := Load(ctx, root, false)
	if e != nil {
		t.Fatal(e)
	}
	if len(got.Quotas) != 1 || len(got.Events) != 1 || len(got.Costs) != 1 || len(got.Contexts) != 1 {
		t.Fatalf("duplicate imports: %+v", got)
	}
	conflict := sample(now)
	conflict.Events[0].Quantities["output"] = 100
	conflict.Quotas[0].UsedPercent = ptr(99.0)
	conflict.Quotas[0].ObservedAt = now.Add(time.Second)
	if e = Import(ctx, root, conflict); e == nil {
		t.Fatal("conflicting request accepted")
	}
	got, e = Load(ctx, root, false)
	if e != nil || *got.Quotas[0].UsedPercent != 20 {
		t.Fatalf("failed import was not atomic: %+v %v", got, e)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			v := sample(now)
			v.Events[0].RequestID = fmt.Sprintf("parallel-%d", i)
			errs <- Import(ctx, root, v)
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	got, e = Load(ctx, root, false)
	if e != nil || len(got.Events) != 9 {
		t.Fatalf("lost concurrent events: %d %v", len(got.Events), e)
	}
}

func TestNewestNullAndResetBoundaries(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Now().UTC()
	s := sample(now)
	s.Quotas[0].ResetAt = &now
	if e := Import(ctx, dir, s); e != nil {
		t.Fatal(e)
	}
	got, e := readSnapshot(ctx, dir, now)
	if e != nil || !got.Quotas[0].Stale {
		t.Fatalf("reset boundary not stale: %+v %v", got.Quotas, e)
	}
	s.Quotas[0].ObservedAt = now.Add(time.Second)
	s.Quotas[0].UsedPercent = nil
	s.Quotas[0].ResetAt = nil
	if e = Import(ctx, dir, s); e != nil {
		t.Fatal(e)
	}
	if e = Import(ctx, dir, sample(now.Add(-time.Hour))); e != nil {
		t.Fatal(e)
	}
	got, e = readSnapshot(ctx, dir, now.Add(time.Second))
	if e != nil || got.Quotas[0].UsedPercent != nil || got.Quotas[0].ResetAt != nil {
		t.Fatalf("unknown overwritten by old observation: %+v %v", got.Quotas, e)
	}
}

func TestClaudeNullWindowsSessionCostAndContext(t *testing.T) {
	now := time.Now().UTC()
	raw := `{"session_id":"abc","model":{"id":"claude-test"},"rate_limits":{"five_hour":{"used_percentage":38,"resets_at":2000000000},"spend_limit":{"used_percentage":112,"resets_at":2000000001}},"context_window":{"context_window_size":200000,"used_percentage":0.15,"current_usage":{"input_tokens":100,"cache_read_input_tokens":150,"cache_creation_input_tokens":50,"output_tokens":30}},"cost":{"total_cost_usd":1.25,"total_duration_ms":2000},"transcript_path":"ignored"}`
	s, e := ParseClaudeStatusline(strings.NewReader(raw), "work", now)
	if e != nil {
		t.Fatal(e)
	}
	if len(s.Quotas) != 3 || s.Quotas[1].UsedPercent != nil || *s.Quotas[2].UsedPercent != 112 {
		t.Fatalf("null/spend window lost: %+v", s.Quotas)
	}
	if *s.Contexts[0].UsedTokens != 300 || s.Costs[0].Kind != "estimate" {
		t.Fatalf("cache doubled or cost mislabeled: %+v", s)
	}
	dir := t.TempDir()
	if e = Import(context.Background(), dir, s); e != nil {
		t.Fatal(e)
	}
	s2, e := ParseClaudeStatusline(strings.NewReader(raw), "work", now.Add(time.Second))
	if e != nil {
		t.Fatal(e)
	}
	if e = Import(context.Background(), dir, s2); e != nil {
		t.Fatal(e)
	}
	got, e := Load(context.Background(), dir, false)
	if e != nil || len(got.Costs) != 1 {
		t.Fatalf("cumulative session counter double counted: %d %v", len(got.Costs), e)
	}
	unknown, e := ParseClaudeStatusline(strings.NewReader(`{"session_id":"abc"}`), "work", now.Add(2*time.Second))
	if e != nil {
		t.Fatal(e)
	}
	if e = Import(context.Background(), dir, unknown); e != nil {
		t.Fatal(e)
	}
	got, e = Load(context.Background(), dir, false)
	if e != nil {
		t.Fatal(e)
	}
	for _, q := range got.Quotas {
		if q.UsedPercent != nil {
			t.Fatal("absent window retained old value")
		}
	}
}

func TestCodexMultiBucketAndAbsentWindow(t *testing.T) {
	now := time.Now().UTC()
	raw := []byte(`{"rateLimits":{"primary":{"usedPercent":99}},"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":25,"windowDurationMins":300,"resetsAt":2000000000},"secondary":null},"review":{"primary":{"usedPercent":0,"windowDurationMins":60},"secondary":null}}}`)
	s, e := ParseCodexRateLimits(raw, "me", now)
	if e != nil {
		t.Fatal(e)
	}
	if len(s.Quotas) != 4 {
		t.Fatalf("legacy view duplicated: %+v", s.Quotas)
	}
	if *s.Quotas[0].UsedPercent != 25 || s.Quotas[1].UsedPercent != nil || *s.Quotas[2].UsedPercent != 0 {
		t.Fatalf("wrong bucket mapping: %+v", s.Quotas)
	}
	if _, e = ParseCodexRateLimits([]byte(`{"rateLimits":null}`), "me", now); e == nil {
		t.Fatal("no quota treated as success")
	}
}

func TestContextEstimatesNeverChangeObservedOccupancy(t *testing.T) {
	now := time.Now().UTC()
	s := sample(now)
	s.Contexts[0].Components = []ContextComponent{{Name: "history", Tokens: 200, Estimated: true}, {Name: "tools", Tokens: 100, Estimated: true}}
	if e := s.Normalize(); e != nil {
		t.Fatal(e)
	}
	if *s.Contexts[0].UsedTokens != 120 || *s.Contexts[0].ComponentDelta != 180 {
		t.Fatalf("estimates rescaled: %+v", s.Contexts[0])
	}
	s.Contexts[0].Components[0].Estimated = false
	s.Contexts[0].Components[1].Estimated = false
	if e := s.Normalize(); e == nil {
		t.Fatal("impossible measured partition accepted")
	}
}

func TestEffectiveCostsScopesCurrenciesAndAdjacentWindows(t *testing.T) {
	now := time.Now().UTC()
	report := sample(now).Costs[0]
	report.Provider = "google"
	report.AccountID = "project"
	estimate := report
	estimate.Kind = "estimate"
	estimate.Scope = "session:abc"
	eur := estimate
	eur.Currency = "EUR"
	next := report
	next.StartAt = report.EndAt
	next.EndAt = report.EndAt.Add(time.Hour)
	other := estimate
	other.AccountID = "other"
	got := EffectiveCosts([]CostObservation{estimate, report, eur, next, other})
	if len(got) != 4 {
		t.Fatalf("wrong coverage suppression: %+v", got)
	}
	for _, c := range got {
		if c.Kind == "estimate" && c.AccountID == "project" && c.Currency == "USD" {
			t.Fatal("estimate doubled report")
		}
	}
}

func TestValidationAndImportBounds(t *testing.T) {
	for _, raw := range []string{`{"version":2}`, `{"version":1,"unknown":true}`, `{"version":1} {}`, strings.Repeat(" ", MaxImportBytes+1)} {
		if _, e := DecodeSnapshot(strings.NewReader(raw)); e == nil {
			t.Fatal("invalid input accepted")
		}
	}
	s := sample(time.Now().UTC())
	s.Quotas[0].AccountID = "bad\x1b[2J"
	if e := s.Normalize(); e == nil {
		t.Fatal("terminal control sequence accepted")
	}
}

func TestPricesAreExplicitDatedAndPartial(t *testing.T) {
	now := time.Now().UTC()
	event := sample(now).Events[0]
	book := PriceBook{Provider: "google", Model: "test-model", Currency: "USD", Source: "provider price sheet 2026-10-02", EffectiveAt: now.Add(-time.Hour), Rates: map[string]UnitRate{"input_uncached": {Amount: "1", PerUnits: 1000000}, "input_cache_read": {Amount: "0.1", PerUnits: 1000000}, "output": {Amount: "2", PerUnits: 1000000}}}
	cost, e := Estimate(event, book)
	if e != nil || cost.Amount != "0.000162" || cost.Partial || cost.Kind != "estimate" {
		t.Fatalf("%+v %v", cost, e)
	}
	delete(book.Rates, "output")
	cost, e = Estimate(event, book)
	if e != nil || !cost.Partial {
		t.Fatalf("unpriced output not marked: %+v %v", cost, e)
	}
	book.EffectiveAt = now.Add(time.Hour)
	if _, e = Estimate(event, book); e == nil {
		t.Fatal("future price applied")
	}
}

func TestCostsPaginationCurrencyAndCents(t *testing.T) {
	now := time.Now().UTC()
	for _, kind := range []string{"openai-costs", "anthropic-costs"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if kind == "openai-costs" && r.Header.Get("Authorization") != "Bearer synthetic" {
					t.Error("missing OpenAI auth")
				}
				if kind == "anthropic-costs" && (r.Header.Get("x-api-key") != "synthetic" || r.Header.Get("anthropic-version") == "") {
					t.Error("missing Anthropic auth")
				}
				if calls == 2 && r.URL.Query().Get("page") != "next" {
					t.Error("cursor not followed")
				}
				more := calls == 1
				if kind == "openai-costs" {
					fmt.Fprintf(w, `{"data":[{"start_time":%d,"end_time":%d,"results":[{"amount":{"value":1.25,"currency":"usd"}}]}],"has_more":%t,"next_page":"next"}`, now.Add(time.Duration(calls-3)*time.Hour).Unix(), now.Add(time.Duration(calls-2)*time.Hour).Unix(), more)
				} else {
					fmt.Fprintf(w, `{"data":[{"starting_at":%q,"ending_at":%q,"results":[{"amount":"123.78912","currency":"USD"}]}],"has_more":%t,"next_page":"next"}`, now.Add(time.Duration(calls-3)*time.Hour).Format(time.RFC3339), now.Add(time.Duration(calls-2)*time.Hour).Format(time.RFC3339), more)
				}
			}))
			defer server.Close()
			provider := "openai"
			if kind == "anthropic-costs" {
				provider = "anthropic"
			}
			s, e := fetchCosts(context.Background(), Connection{Kind: kind, Provider: provider, AccountID: "org"}, "synthetic", now, server.Client(), server.URL)
			if e != nil {
				t.Fatal(e)
			}
			if calls != 2 || len(s.Costs) != 2 {
				t.Fatalf("pagination: %d %+v", calls, s)
			}
			expected := "1.25"
			if kind == "anthropic-costs" {
				expected = "1.2378912"
			}
			if s.Costs[0].Amount != expected || s.Costs[0].Currency != "USD" {
				t.Fatalf("wrong money units: %+v", s.Costs[0])
			}
		})
	}
}

func TestHTTPFailureRetryCancellationAndNoSecrets(t *testing.T) {
	now := time.Now().UTC()
	c := Connection{Kind: "openai-costs", Provider: "openai", AccountID: "org"}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(429)
			return
		}
		fmt.Fprint(w, `{"data":[],"has_more":false}`)
	}))
	defer server.Close()
	if _, e := fetchCosts(context.Background(), c, "synthetic-secret", now, server.Client(), server.URL); e != nil || calls != 2 {
		t.Fatalf("retry failed: %d %v", calls, e)
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		fmt.Fprint(w, "synthetic-secret body")
	}))
	defer bad.Close()
	_, e := fetchCosts(context.Background(), c, "synthetic-secret", now, bad.Client(), bad.URL)
	if e == nil || strings.Contains(e.Error(), "synthetic-secret") {
		t.Fatalf("error leaked or missing: %v", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e = fetchCosts(ctx, c, "synthetic", now, server.Client(), server.URL); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancellation: %v", e)
	}
}

func TestInvalidPaginationAndNullAmount(t *testing.T) {
	for _, body := range []string{`{"data":[],"has_more":true,"next_page":null}`, `{"data":[{"start_time":1,"end_time":2,"results":[{"amount":null}]}],"has_more":false}`, `{"data":[],"has_more":true,"next_page":"same"}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		_, e := fetchCosts(context.Background(), Connection{Kind: "openai-costs", Provider: "openai", AccountID: "org"}, "synthetic", time.Now().UTC(), server.Client(), server.URL)
		server.Close()
		if e == nil {
			t.Fatalf("bad provider response accepted: %s", body)
		}
	}
}

func TestRefreshOptInFailureRetainsStaleData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Now().UTC()
	s := sample(now)
	s.Quotas[0].AccountID = "org"
	s.Costs[0].Source = "openai-costs"
	if e := Import(ctx, dir, s); e != nil {
		t.Fatal(e)
	}
	config := Config{Version: Version, Connections: []Connection{{ID: "bill", Provider: "openai", Kind: "openai-costs", AccountID: "org", APIKeyEnv: "GRAYMATTER_TEST_UNSET_ADMIN", Enabled: true}}}
	t.Setenv("GRAYMATTER_TEST_UNSET_ADMIN", "")
	if e := SaveConfig(dir, config); e != nil {
		t.Fatal(e)
	}
	local, e := Load(ctx, dir, false)
	if e != nil || len(local.Connections) != 1 || local.Connections[0].LastAttempt != nil {
		t.Fatalf("local read contacted provider: %+v %v", local, e)
	}
	got, e := Load(ctx, dir, true)
	if e == nil || got.Connections[0].State != "error" || len(got.Costs) != 1 || !got.Costs[0].Stale {
		t.Fatalf("failed refresh lost snapshot: %+v %v", got, e)
	}
	data, e := os.ReadFile(ConfigPath(dir))
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Contains(data, []byte("Bearer")) {
		t.Fatal("credential in config")
	}
}

func TestSnapshotJSONPreservesNulls(t *testing.T) {
	s := Snapshot{Version: Version, Quotas: []QuotaSnapshot{{Provider: "google", AccountID: "manual", Source: "import", Window: "daily", ObservedAt: time.Now().UTC()}}}
	b, e := json.Marshal(s)
	if e != nil || !bytes.Contains(b, []byte(`"used_percent":null`)) {
		t.Fatalf("null lost: %s %v", b, e)
	}
}

func TestEffectiveCostsPartialTemporalOverlap(t *testing.T) {
	now := time.Now().UTC()
	report := sample(now).Costs[0]
	estimate := report
	estimate.Kind = "estimate"
	estimate.Scope = "session:long"
	estimate.StartAt = report.StartAt.Add(-time.Hour)
	estimate.EndAt = report.EndAt.Add(time.Hour)
	got := EffectiveCosts([]CostObservation{estimate, report})
	if len(got) != 1 || got[0].Kind != "reported" || !got[0].Partial {
		t.Fatalf("outside coverage hidden: %+v", got)
	}
	estimate.StartAt = report.StartAt
	estimate.EndAt = report.EndAt
	got = EffectiveCosts([]CostObservation{estimate, report})
	if len(got) != 1 || got[0].Partial {
		t.Fatalf("complete coverage marked partial: %+v", got)
	}
}

func TestParallelRefreshPreservesSuccessWhenAnotherProviderCancels(t *testing.T) {
	dir := t.TempDir()
	config := Config{Version: Version, Connections: []Connection{{ID: "slow", Kind: "openai-costs", Provider: "openai", AccountID: "slow", Enabled: true, APIKeyEnv: "UNUSED_TEST_KEY"}, {ID: "fast", Kind: "anthropic-costs", Provider: "anthropic", AccountID: "fast", Enabled: true, APIKeyEnv: "UNUSED_TEST_KEY"}}}
	if e := SaveConfig(dir, config); e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	e := refreshWith(ctx, dir, func(ctx context.Context, c Connection, now time.Time) (Snapshot, error) {
		if c.ID == "slow" {
			<-ctx.Done()
			return Snapshot{}, ctx.Err()
		}
		cost := sample(now).Costs[0]
		cost.Provider = c.Provider
		cost.AccountID = c.AccountID
		cost.Source = c.Kind
		return Snapshot{Version: Version, Costs: []CostObservation{cost}}, nil
	})
	if !errors.Is(e, context.DeadlineExceeded) {
		t.Fatalf("missing failure: %v", e)
	}
	s, e := Load(context.Background(), dir, false)
	if e != nil || len(s.Costs) != 1 || s.Costs[0].AccountID != "fast" || s.Costs[0].Stale {
		t.Fatalf("successful provider starved: %+v %v", s, e)
	}
	states := map[string]string{}
	for _, c := range s.Connections {
		states[c.ID] = c.State
	}
	if states["slow"] != "error" || states["fast"] != "ready" {
		t.Fatalf("per-provider states lost: %v", states)
	}
}

func TestReadFailureNeverReturnsUsablePartialSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	dir := t.TempDir()
	if e := Import(context.Background(), dir, sample(time.Now().UTC())); e != nil {
		t.Fatal(e)
	}
	cancel()
	s, e := Load(ctx, dir, false)
	if e == nil || s.Version != 0 {
		t.Fatalf("canceled scan looks usable: %+v %v", s, e)
	}
}

func TestProviderDecimalPrecisionAndNullSchema(t *testing.T) {
	now := time.Now().UTC()
	c := Connection{Kind: "openai-costs", Provider: "openai", AccountID: "org"}
	costs, e := parseCostBucket([]byte(`{"start_time":1,"end_time":2,"results":[{"amount":{"value":1.25e-13,"currency":"usd"}},{"amount":{"value":-0.01,"currency":"usd"}}]}`), c, now)
	if e != nil || len(costs) != 1 || costs[0].Amount != "-0.009999999999875" {
		t.Fatalf("lost subcent precision/credit: %+v %v", costs, e)
	}
	for _, body := range []string{`{}`, `{"data":null,"has_more":false}`, `{"data":[],"has_more":null}`} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, body) }))
		_, e := fetchCosts(context.Background(), c, "synthetic", now, server.Client(), server.URL)
		server.Close()
		if e == nil {
			t.Fatalf("missing amount treated as zero: %s", body)
		}
	}
}

func TestCodexRemovedBucketsBecomeUnknown(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	s, e := ParseCodexRateLimits([]byte(`{"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":20}},"old-bucket":{"primary":{"usedPercent":90}}}}`), "personal", now)
	if e != nil {
		t.Fatal(e)
	}
	if e = Import(context.Background(), dir, s); e != nil {
		t.Fatal(e)
	}
	if e = SaveConfig(dir, Config{Version: Version, Connections: []Connection{{ID: "codex", Kind: "codex-app-server", Provider: "openai", AccountID: "personal", Enabled: true, Command: []string{"synthetic-not-run"}}}}); e != nil {
		t.Fatal(e)
	}
	if e = refreshWith(context.Background(), dir, func(_ context.Context, _ Connection, at time.Time) (Snapshot, error) {
		return ParseCodexRateLimits([]byte(`{"rateLimitsByLimitId":{"codex":{"primary":{"usedPercent":21}}}}`), "personal", at)
	}); e != nil {
		t.Fatal(e)
	}
	got, e := Load(context.Background(), dir, false)
	if e != nil {
		t.Fatal(e)
	}
	for _, q := range got.Quotas {
		if q.Label == "old-bucket" && q.UsedPercent != nil {
			t.Fatal("removed provider bucket retained a fresh percentage")
		}
	}
}

func TestSourceFailureDoesNotInvalidateOtherAccountData(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	now := time.Now().UTC()
	s := sample(now)
	s.Quotas[0].Source = "codex-app-server"
	s.Quotas[0].AccountID = "shared-label"
	s.Costs[0].AccountID = "shared-label"
	s.Costs[0].Source = "openai-costs"
	s.Connections = []ConnectionStatus{{ID: "costs", Provider: "openai", AccountID: "shared-label", Kind: "openai-costs", State: "error", Error: "synthetic", LastAttempt: &now}}
	if e := Import(ctx, dir, s); e != nil {
		t.Fatal(e)
	}
	got, e := Load(ctx, dir, false)
	if e != nil || got.Quotas[0].Stale || !got.Costs[0].Stale {
		t.Fatalf("unrelated source invalidated: %+v %v", got, e)
	}
}

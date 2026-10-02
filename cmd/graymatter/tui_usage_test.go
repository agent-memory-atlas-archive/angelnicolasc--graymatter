package main

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/usage"
)

func usageFixture() usage.Snapshot {
	percent := 54.0
	used, limit := int64(1200), int64(200000)
	now := time.Now().UTC()
	reset := now.Add(time.Hour)
	return usage.Snapshot{
		Quotas: []usage.QuotaSnapshot{{ID: "quota-1", Provider: "provider-a", AccountID: "account-a", Window: "5h", UsedPercent: &percent, ResetAt: &reset, Source: "fixture", ObservedAt: now}},
		Costs: []usage.CostObservation{
			{ID: "cost-1", Provider: "provider-a", AccountID: "org-a", Amount: "12.48", Currency: "usd", Kind: "reported", Scope: "project-a", StartAt: now.Add(-time.Hour), EndAt: now, ObservedAt: now},
			{ID: "cost-2", Provider: "provider-b", AccountID: "org-b", Amount: "4.16", Currency: "eur", Kind: "estimate", Scope: "project-b", StartAt: now.Add(-time.Hour), EndAt: now, Partial: true, ObservedAt: now},
		},
		Contexts: []usage.ContextSnapshot{{Provider: "harness", SessionID: "session-a", Source: "request", UsedTokens: &used, LimitTokens: &limit, Estimated: true, ObservedAt: now, Components: []usage.ContextComponent{{Name: "Instructions", Tokens: 1200, Estimated: true}}}},
	}
}

func TestUsageModesKeepQuotaSpendAndContextSeparate(t *testing.T) {
	m := tuiModel{}
	m.initUsage()
	m.usage.loaded = true
	m.usage.snapshot = usageFixture()
	for _, tc := range []struct {
		mode         string
		quota, spend bool
	}{{"auto", true, true}, {"limits", true, false}, {"spend", false, true}} {
		t.Run(tc.mode, func(t *testing.T) {
			m.usage.mode = tc.mode
			out := ansi.Strip(strings.Join(m.usageLines(100), "\n"))
			if strings.Contains(out, "Subscription limits") != tc.quota || strings.Contains(out, "API spend") != tc.spend {
				t.Fatalf("incorrect mode: %s", out)
			}
			if !strings.Contains(out, "Context") {
				t.Fatal("context disappeared when changing billing display")
			}
			if tc.spend && (!strings.Contains(out, "12.48 USD") || !strings.Contains(out, "4.16 EUR") || strings.Contains(out, "16.64")) {
				t.Fatalf("currencies or measurement methods were merged: %s", out)
			}
		})
	}
}

func TestUsageMissingDataNeverLooksLikeZero(t *testing.T) {
	m := tuiModel{}
	m.initUsage()
	m.usage.loaded = true
	m.usage.snapshot = usage.Snapshot{Quotas: []usage.QuotaSnapshot{{ID: "unknown", Provider: "not-connected", Window: "5h"}}}
	out := strings.Join(m.usageLines(90), "\n")
	for _, wrong := range []string{"0% used", "$0", "0.00 USD"} {
		if strings.Contains(out, wrong) {
			t.Fatalf("missing source masquerades as zero: %s", out)
		}
	}
	if !strings.Contains(out, "—") || !strings.Contains(out, "reset unknown") {
		t.Fatalf("missing values unlabelled: %s", out)
	}
}

func TestUsageLateResultsAndSourceFailurePreserveIdentity(t *testing.T) {
	m := tuiModel{}
	m.initUsage()
	m.usage.generation = 2
	m.usage.sources[0] = usageFixture()
	m.usage.snapshot = usageFixture()
	m.updateUsage(tuiUsageLoadedMsg{generation: 1})
	if len(m.usage.snapshot.Quotas) != 1 {
		t.Fatal("old request replaced current account")
	}
	m.updateUsage(tuiUsageLoadedMsg{generation: 2, errors: [2]string{"provider unavailable", ""}})
	if len(m.usage.snapshot.Quotas) != 1 || !m.usage.snapshot.Quotas[0].Stale || *m.usage.snapshot.Quotas[0].UsedPercent != 54 {
		t.Fatal("failed refresh lost last valid quota")
	}
	out := strings.Join(m.usageLines(100), "\n")
	if !strings.Contains(out, "provider unavailable") || !strings.Contains(out, "stale") {
		t.Fatal("source failure is hidden")
	}
}

func TestUsageControlsAndCellBounds(t *testing.T) {
	m := tuiModel{height: 24}
	m.initUsage()
	m.usage.snapshot = usageFixture()
	m.usage.loaded = true
	for _, key := range []rune{'c', 'v', 's'} {
		m.updateUsage(tea.KeyPressMsg{Code: key, Text: string(key)})
	}
	if !m.usage.showContext || !m.usage.showSources || m.usage.mode != "spend" {
		t.Fatal("Usage controls did not update state")
	}
	for _, size := range [][2]int{{40, 10}, {80, 18}, {100, 24}, {120, 30}, {160, 42}} {
		out := m.renderUsage(size[0], size[1])
		lines := strings.Split(out, "\n")
		if len(lines) > size[1] {
			t.Fatalf("%dx%d overflow: %d lines", size[0], size[1], len(lines))
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size[0] {
				t.Fatalf("row overflow at %d: %q", size[0], line)
			}
		}
	}
}

func TestUsagePartialRefreshKeepsSuccessfulSources(t *testing.T) {
	m := tuiModel{width: 80, height: 24}
	m.initUsage()
	m.usage.generation = 3
	snapshot := usageFixture()
	snapshot.Version = usage.Version
	value := 61.0
	snapshot.Quotas[0].UsedPercent = &value
	snapshot.Connections = []usage.ConnectionStatus{{ID: "billing", State: "error", Error: "service unavailable"}}
	m.updateUsage(tuiUsageLoadedMsg{generation: 3, sources: [2]usage.Snapshot{snapshot}, errors: [2]string{"billing: service unavailable"}})
	if got := m.usage.snapshot.Quotas; len(got) != 1 || *got[0].UsedPercent != value || got[0].Stale {
		t.Fatalf("partial failure discarded successful quota: %+v", got)
	}
	if m.usage.errors[0] == "" || len(m.usage.snapshot.Connections) != 1 {
		t.Fatal("partial failure diagnostics were lost")
	}
}

func TestUsageEndThenUpAndResizeScrollImmediately(t *testing.T) {
	m := tuiModel{width: 80, height: 24}
	m.initUsage()
	m.usage.snapshot = usageFixture()
	m.usage.loaded = true
	m.usage.showContext = true
	m.updateUsage(tea.KeyPressMsg{Code: tea.KeyEnd})
	end := m.usage.offset
	if end == 0 || end != m.usageMaxOffset() {
		t.Fatalf("end did not select final page: %d", end)
	}
	m.updateUsage(tea.KeyPressMsg{Code: tea.KeyUp})
	if m.usage.offset != end-1 {
		t.Fatal("up after end did not move one row")
	}
	m.height = 100
	m.updateUsage(tea.KeyPressMsg{Code: tea.KeyDown})
	if m.usage.offset != 0 {
		t.Fatal("resize left offset beyond visible content")
	}
}

func TestUsageDemoRefreshNeverInvokesConfiguredAccountSources(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GRAYMATTER_TEST_DEMO_ADMIN_KEY", "")
	if err := usage.SaveConfig(dir, usage.Config{Version: usage.Version, Connections: []usage.Connection{{ID: "external", Provider: "openai", Kind: "openai-costs", AccountID: "sample", Enabled: true, APIKeyEnv: "GRAYMATTER_TEST_DEMO_ADMIN_KEY"}}}); err != nil {
		t.Fatal(err)
	}
	m := tuiModel{demo: true, width: 80, height: 24}
	m.initUsage()
	m.usage.stateDir = dir
	result := m.refreshUsage()().(tuiUsageLoadedMsg)
	if result.errors[0] != "" || len(result.sources[0].Connections) != 1 || result.sources[0].Connections[0].LastAttempt != nil {
		t.Fatalf("demo attempted account refresh: %+v", result.sources[0].Connections)
	}
}

func TestUsageDisplayBoundsLargeLedgerAndOrdersRecentEvents(t *testing.T) {
	s := usageFixture()
	for i := 0; i < 150; i++ {
		c := s.Costs[0]
		c.ID = fmt.Sprintf("cost-%03d", i)
		c.Scope = fmt.Sprintf("request:%d", i)
		c.StartAt = c.StartAt.Add(time.Duration(i) * time.Hour)
		c.EndAt = c.StartAt.Add(time.Minute)
		s.Costs = append(s.Costs, c)
	}
	s.Events = []usage.UsageEvent{{ID: "earlier", Time: time.Unix(1, 0)}, {ID: "later", Time: time.Unix(2, 0)}}
	view := prepareUsageDisplay([2]usage.Snapshot{s}, [2]string{}, [2]usage.Snapshot{})
	if len(view.Costs) != 100 || len(view.Warnings) == 0 || view.Events[0].ID != "later" {
		t.Fatal("large ledger was not bounded or recent events were not ordered")
	}
}

func TestUsageUntrustedLabelsCannotEmitTerminalControls(t *testing.T) {
	m := tuiModel{}
	m.initUsage()
	m.usage.loaded = true
	m.usage.snapshot = usageFixture()
	m.usage.snapshot.Quotas[0].Provider = "\x1b]52;c;c2VjcmV0\aevil\x1b[2J"
	out := strings.Join(m.usageLines(100), "\n")
	if strings.Contains(out, "\x1b]52") || strings.Contains(out, "\x1b[2J") || strings.Contains(out, "\a") {
		t.Fatal("provider metadata injected terminal commands")
	}
}

func TestUsageWindowsUnknownExpiredAndMalformed(t *testing.T) {
	now := time.Now()
	past := now.Add(-time.Second)
	if usageReset(nil, now) != "reset unknown" || usageReset(&past, now) != "reset passed · refresh" {
		t.Fatal("missing or expired window interpreted as fresh")
	}
	if usageBar(nil, 10) != strings.Repeat("·", 10) {
		t.Fatal("missing percentage has a filled numeric meter")
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), -1, 101} {
		bar := usageBar(&value, 10)
		if ansi.StringWidth(bar) != 10 {
			t.Fatalf("invalid percentage broke width: %v", value)
		}
	}
}

func TestUsageCompactPresentationPreservesAccountAndFailures(t *testing.T) {
	m := tuiModel{}
	m.initUsage()
	m.setTheme("dark")
	m.usage.loaded = true
	m.usage.snapshot = usageFixture()
	account := "organization-with-a-long-unique-account-suffix-12345"
	m.usage.snapshot.Costs[0].AccountID = account
	m.usage.snapshot.Quotas[0].Label = "Production organization subscription"
	m.usage.snapshot.Quotas[0].Window = "monthly-calendar"
	m.usage.snapshot.Contexts[0].Components[0].Name = "input_cache_read"
	m.usage.showContext = true
	m.usage.snapshot.Connections = []usage.ConnectionStatus{{Provider: "billing", AccountID: "work", State: "error", Error: "permission denied"}}
	for _, width := range []int{36, 76, 96, 132} {
		out := ansi.Strip(strings.Join(m.usageLines(width), "\n"))
		// Whitespace introduced by wrapping must not erase account identity.
		compact := strings.Join(strings.Fields(out), "")
		for _, required := range []string{account, "Productionorganizationsubscription", "monthly-calendar", "input_cache_read", "permissiondenied"} {
			if !strings.Contains(compact, required) {
				t.Fatalf("compact provenance hid %q at %d: %s", required, width, out)
			}
		}
		if strings.Contains(out, "config.json") {
			t.Fatal("configuration path occupies collapsed overview")
		}
	}
	m.usage.showSources = true
	out := ansi.Strip(strings.Join(m.usageLines(132), "\n"))
	if !strings.Contains(out, "config.json") {
		t.Fatal("expanded source configuration unavailable")
	}
}

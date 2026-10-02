package harness

import (
	"context"
	"fmt"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/usage"
	"github.com/anthropics/anthropic-sdk-go"
)

func recordUsageObservation(ctx context.Context, cfg RunConfig, model, sessionID string, attempt int, msg *anthropic.Message, system, memory string, history []session.Message, task string) error {
	account := cfg.UsageAccountID
	if account == "" {
		account = "local-harness"
	}
	request := msg.ID
	if request == "" {
		request = fmt.Sprintf("%s/response/%d", sessionID, attempt)
	}
	if string(msg.Model) != "" {
		model = string(msg.Model)
	}
	now := time.Now().UTC()
	e := usage.UsageEvent{Provider: "anthropic", Source: "graymatter-harness", AccountID: account, RequestID: request, SessionID: sessionID, Model: model, Time: now, Operation: "messages", Quantities: map[string]int64{"input_uncached": msg.Usage.InputTokens, "output": msg.Usage.OutputTokens, "input_cache_read": msg.Usage.CacheReadInputTokens, "input_cache_write": msg.Usage.CacheCreationInputTokens, "requests": 1}}
	cache1h, cache5m := msg.Usage.CacheCreation.Ephemeral1hInputTokens, msg.Usage.CacheCreation.Ephemeral5mInputTokens
	if cache1h > 0 || cache5m > 0 {
		delete(e.Quantities, "input_cache_write")
		e.Quantities["input_cache_write_1h"] = cache1h
		e.Quantities["input_cache_write_5m"] = cache5m
		if remainder := msg.Usage.CacheCreationInputTokens - cache1h - cache5m; remainder > 0 {
			e.Quantities["input_cache_write_unknown"] = remainder
		}
	}
	input := msg.Usage.InputTokens + msg.Usage.CacheReadInputTokens + msg.Usage.CacheCreationInputTokens
	total := input + msg.Usage.OutputTokens
	// Component sizes cannot be attributed by the API. Text-length heuristics
	// remain separate estimates; they are never rescaled to the observed total.
	// Cache counters are billing categories, not extra context components.
	var historyBytes int
	for _, m := range history {
		historyBytes += len(m.Content)
	}
	lengths := []int{len(system), len(memory), historyBytes, len(task)}
	names := []string{"system", "memory", "history", "task"}
	parts := []usage.ContextComponent{}
	for i, n := range lengths {
		if n == 0 {
			continue
		}
		tokens := int64((n + 3) / 4)
		parts = append(parts, usage.ContextComponent{Name: names[i], Tokens: tokens, Estimated: true})
	}
	parts = append(parts, usage.ContextComponent{Name: "output", Tokens: msg.Usage.OutputTokens})
	x := usage.ContextSnapshot{Provider: "anthropic", AccountID: account, SessionID: sessionID, Model: model, Source: "graymatter-harness", UsedTokens: &total, Components: parts, ObservedAt: now}
	s := usage.Snapshot{Version: usage.Version, GeneratedAt: now, Events: []usage.UsageEvent{e}, Contexts: []usage.ContextSnapshot{x}}
	// Existing model price table remains an explicitly partial local estimate.
	// Unknown models keep tokens/context and have no fabricated dollar amount.
	if price, ok := LookupPricing(model); ok {
		cost := price.CostUSD(uint64(msg.Usage.InputTokens), uint64(msg.Usage.OutputTokens), uint64(msg.Usage.CacheReadInputTokens), uint64(msg.Usage.CacheCreationInputTokens))
		// The legacy price table assumes five-minute writes; account for an
		// explicitly reported one-hour TTL at 2x the ordinary input rate.
		if cache1h > 0 {
			cost += float64(cache1h) * (2*price.InputPer1M - price.CacheWritePer1M) / 1e6
		}
		s.Costs = []usage.CostObservation{{Provider: "anthropic", AccountID: account, Amount: fmt.Sprintf("%.12f", cost), Currency: "USD", Kind: "estimate", Scope: "request:" + request, Source: "graymatter-harness-model-prices-2026-08-10", StartAt: now, EndAt: now.Add(time.Nanosecond), ObservedAt: now, Partial: true}}
	}
	return usage.Import(ctx, cfg.DataDir, s)
}

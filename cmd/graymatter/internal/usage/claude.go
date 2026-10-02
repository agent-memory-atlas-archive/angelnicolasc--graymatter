package usage

import (
	"fmt"
	"io"
	"strconv"
	"time"
)

// ParseClaudeStatusline consumes Claude Code's supported statusline JSON on
// stdin. It intentionally ignores transcript paths, workspace paths and auth.
// See https://code.claude.com/docs/en/statusline#rate-limit-usage.
func ParseClaudeStatusline(r io.Reader, accountID string, observedAt time.Time) (Snapshot, error) {
	var v struct {
		SessionID string `json:"session_id"`
		Model     struct {
			ID string `json:"id"`
		} `json:"model"`
		RateLimits map[string]*struct {
			Used  *float64 `json:"used_percentage"`
			Reset *int64   `json:"resets_at"`
		} `json:"rate_limits"`
		Context struct {
			Used    *float64 `json:"used_percentage"`
			Size    *int64   `json:"context_window_size"`
			Current *struct {
				Input  int64 `json:"input_tokens"`
				Output int64 `json:"output_tokens"`
				Read   int64 `json:"cache_read_input_tokens"`
				Write  int64 `json:"cache_creation_input_tokens"`
			} `json:"current_usage"`
		} `json:"context_window"`
		Cost struct {
			Total    *float64 `json:"total_cost_usd"`
			Duration *int64   `json:"total_duration_ms"`
		} `json:"cost"`
	}
	s := Snapshot{Version: Version, GeneratedAt: observedAt}
	if accountID == "" {
		return s, fmt.Errorf("explicit account ID required")
	}
	if e := decodeJSON(r, &v, MaxImportBytes, false); e != nil {
		return s, e
	}
	if v.SessionID == "" {
		return s, fmt.Errorf("Claude statusline requires session_id")
	}
	// Write explicit unknown snapshots for absent windows so an older window
	// cannot survive a later snapshot after reset or account-plan changes.
	for _, window := range []string{"five_hour", "seven_day", "spend_limit"} {
		q := QuotaSnapshot{Provider: "anthropic", AccountID: accountID, Source: "claude-statusline", Label: "Claude", Window: window, ObservedAt: observedAt}
		if w := v.RateLimits[window]; w != nil {
			q.UsedPercent = w.Used
			if w.Reset != nil {
				t := time.Unix(*w.Reset, 0).UTC()
				q.ResetAt = &t
			}
		}
		s.Quotas = append(s.Quotas, q)
	}
	x := ContextSnapshot{Provider: "anthropic", AccountID: accountID, SessionID: v.SessionID, Model: v.Model.ID, Source: "claude-statusline", UsedPercent: v.Context.Used, LimitTokens: v.Context.Size, ObservedAt: observedAt}
	if c := v.Context.Current; c != nil {
		n := c.Input + c.Read + c.Write
		x.UsedTokens = &n
		x.Components = []ContextComponent{{Name: "input_uncached", Tokens: c.Input}, {Name: "input_cache_read", Tokens: c.Read}, {Name: "input_cache_write", Tokens: c.Write}}
	}
	s.Contexts = append(s.Contexts, x)
	// Claude Code's local dollar counter is list-price equivalent usage, even
	// for subscriptions. It is NEVER a provider-reported payment observation.
	if v.Cost.Total != nil {
		start := observedAt
		if v.Cost.Duration != nil && *v.Cost.Duration > 0 {
			start = observedAt.Add(-time.Duration(*v.Cost.Duration) * time.Millisecond)
		} else {
			start = observedAt.Add(-time.Nanosecond)
		}
		s.Costs = append(s.Costs, CostObservation{Provider: "anthropic", AccountID: accountID, Amount: strconv.FormatFloat(*v.Cost.Total, 'f', -1, 64), Currency: "USD", Kind: "estimate", Scope: "session:" + v.SessionID, Source: "claude-statusline-list-price", StartAt: start, EndAt: observedAt, ObservedAt: observedAt, Partial: true})
	}
	return s, s.Normalize()
}

// Package usage keeps independently sourced account limits, billing observations,
// request events, and context occupancy. Missing values are never implicit zeroes.
package usage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/big"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

const Version = 1

type Snapshot struct {
	Version     int                `json:"version"`
	GeneratedAt time.Time          `json:"generated_at"`
	Quotas      []QuotaSnapshot    `json:"quotas,omitempty"`
	Costs       []CostObservation  `json:"costs,omitempty"`
	Events      []UsageEvent       `json:"events,omitempty"`
	Contexts    []ContextSnapshot  `json:"contexts,omitempty"`
	Connections []ConnectionStatus `json:"connections,omitempty"`
	Warnings    []string           `json:"warnings,omitempty"`
}

type QuotaSnapshot struct {
	ID            string     `json:"id"`
	Provider      string     `json:"provider"`
	AccountID     string     `json:"account_id"`
	Label         string     `json:"label"`
	Window        string     `json:"window"`
	WindowMinutes *int64     `json:"window_minutes,omitempty"`
	Source        string     `json:"source"`
	UsedPercent   *float64   `json:"used_percent"`
	ResetAt       *time.Time `json:"reset_at"`
	ObservedAt    time.Time  `json:"observed_at"`
	Stale         bool       `json:"stale"`
	Error         string     `json:"error,omitempty"`
}

// Amount is a decimal string in major currency units. Kind is reported or
// estimate. Subscription equivalent API prices are estimates, never spend.
// Scope identifies coverage (organization, project:ID, or session:ID).
type CostObservation struct {
	ID         string    `json:"id"`
	Provider   string    `json:"provider"`
	AccountID  string    `json:"account_id"`
	Amount     string    `json:"amount"`
	Currency   string    `json:"currency"`
	Kind       string    `json:"kind"`
	Scope      string    `json:"scope"`
	Source     string    `json:"source"`
	StartAt    time.Time `json:"start_at"`
	EndAt      time.Time `json:"end_at"`
	ObservedAt time.Time `json:"observed_at"`
	Partial    bool      `json:"partial"`
	Stale      bool      `json:"stale"`
}

// Quantities must be disjoint categories. For example input_uncached,
// input_cache_read, input_cache_write, output, requests, or embedding_input.
// ID is derived from source/account/request/operation, not ingestion time.
type UsageEvent struct {
	ID         string           `json:"id"`
	Provider   string           `json:"provider"`
	Source     string           `json:"source"`
	AccountID  string           `json:"account_id"`
	RequestID  string           `json:"request_id"`
	SessionID  string           `json:"session_id,omitempty"`
	Model      string           `json:"model"`
	Time       time.Time        `json:"time"`
	Operation  string           `json:"operation"`
	Quantities map[string]int64 `json:"quantities"`
}

type ContextSnapshot struct {
	Provider       string             `json:"provider"`
	AccountID      string             `json:"account_id"`
	SessionID      string             `json:"session_id"`
	Model          string             `json:"model"`
	Source         string             `json:"source"`
	UsedTokens     *int64             `json:"used_tokens"`
	LimitTokens    *int64             `json:"limit_tokens"`
	UsedPercent    *float64           `json:"used_percent"`
	Components     []ContextComponent `json:"components,omitempty"`
	ComponentDelta *int64             `json:"component_delta,omitempty"` // estimated partition sum minus observed total
	ObservedAt     time.Time          `json:"observed_at"`
	Estimated      bool               `json:"estimated"`
	Stale          bool               `json:"stale"`
}

type ContextComponent struct {
	Name      string `json:"name"`
	Tokens    int64  `json:"tokens"`
	Estimated bool   `json:"estimated"`
}

type ConnectionStatus struct {
	ID          string     `json:"id"`
	Provider    string     `json:"provider"`
	Kind        string     `json:"kind"`
	AccountID   string     `json:"account_id"`
	State       string     `json:"state"`
	Error       string     `json:"error,omitempty"`
	LastSuccess *time.Time `json:"last_success"`
	LastAttempt *time.Time `json:"last_attempt"`
}

func stableID(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		fmt.Fprintf(h, "%d:%s", len(p), p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func EventID(e UsageEvent) string {
	return stableID(e.Provider, e.Source, e.AccountID, e.RequestID, e.Operation)
}
func contextID(c ContextSnapshot) string {
	return stableID(c.Provider, c.AccountID, c.SessionID, c.Source)
}

var decimalPattern = regexp.MustCompile(`^-?[0-9]+(?:\.[0-9]+)?$`)
var currencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)

func decimal(s string) (*big.Rat, error) {
	if len(s) > 100 || !decimalPattern.MatchString(s) {
		return nil, fmt.Errorf("invalid decimal amount")
	}
	n, ok := new(big.Rat).SetString(s)
	if !ok {
		return nil, fmt.Errorf("invalid decimal amount")
	}
	return n, nil
}

// decimalString preserves exact terminating provider decimals, including sub-
// cent charges. Non-terminating user unit rates are estimates rounded to 18
// decimal places rather than binary floating-point arithmetic.
func decimalString(n *big.Rat) string {
	d := new(big.Int).Set(n.Denom())
	two, five := big.NewInt(2), big.NewInt(5)
	digits := 0
	for _, factor := range []*big.Int{two, five} {
		count := 0
		for new(big.Int).Mod(d, factor).Sign() == 0 {
			d.Quo(d, factor)
			count++
		}
		if count > digits {
			digits = count
		}
	}
	if d.Cmp(big.NewInt(1)) != 0 {
		digits = 18
	}
	if digits > 128 {
		digits = 128
	}
	s := n.FloatString(digits)
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

func safeFields(values ...string) bool {
	for _, s := range values {
		if len(s) > 1024 {
			return false
		}
		for _, r := range s {
			if unicode.IsControl(r) {
				return false
			}
		}
	}
	return true
}

// Normalize validates imported observations, assigns stable IDs, and preserves
// explicit null values. It never fills missing quotas, prices, or capacities.
func (s *Snapshot) Normalize() error {
	if s.Version != 0 && s.Version != Version {
		return fmt.Errorf("unsupported usage schema version %d", s.Version)
	}
	s.Version = Version
	if len(s.Events)+len(s.Quotas)+len(s.Costs)+len(s.Contexts) > 10000 {
		return fmt.Errorf("too many observations")
	}
	for i := range s.Quotas {
		q := &s.Quotas[i]
		if !safeFields(q.Provider, q.AccountID, q.Source, q.Label, q.Window, q.Error) {
			return fmt.Errorf("invalid quota text")
		}
		if q.Provider == "" || q.AccountID == "" || q.Source == "" || q.Window == "" || q.ObservedAt.IsZero() {
			return fmt.Errorf("quota requires provider, account, source, window and observed_at")
		}
		if q.UsedPercent != nil && (math.IsNaN(*q.UsedPercent) || math.IsInf(*q.UsedPercent, 0) || *q.UsedPercent < 0) {
			return fmt.Errorf("invalid quota percentage")
		}
		q.ID = stableID(q.Provider, q.AccountID, q.Source, q.Label, q.Window)
	}
	for i := range s.Costs {
		c := &s.Costs[i]
		if !safeFields(c.Provider, c.AccountID, c.Source, c.Scope) {
			return fmt.Errorf("invalid cost text")
		}
		c.Currency = strings.ToUpper(c.Currency)
		if _, err := decimal(c.Amount); err != nil {
			return err
		}
		if !currencyPattern.MatchString(c.Currency) || (c.Kind != "reported" && c.Kind != "estimate") || c.Provider == "" || c.AccountID == "" || c.Source == "" || c.Scope == "" || c.ObservedAt.IsZero() || c.StartAt.IsZero() || !c.EndAt.After(c.StartAt) {
			return fmt.Errorf("invalid cost coverage, provenance, currency or kind")
		}
		c.ID = stableID(c.Provider, c.AccountID, c.Kind, c.Scope, c.Currency, c.StartAt.UTC().Format(time.RFC3339Nano), c.EndAt.UTC().Format(time.RFC3339Nano))
		if strings.HasPrefix(c.Scope, "session:") || strings.HasPrefix(c.Scope, "request:") {
			c.ID = stableID(c.Provider, c.AccountID, c.Kind, c.Scope, c.Currency)
		}
	}
	for i := range s.Events {
		e := &s.Events[i]
		if !safeFields(e.Provider, e.AccountID, e.Source, e.RequestID, e.SessionID, e.Model, e.Operation) {
			return fmt.Errorf("invalid event text")
		}
		if e.Provider == "" || e.Source == "" || e.AccountID == "" || e.RequestID == "" || e.Operation == "" || e.Time.IsZero() {
			return fmt.Errorf("event requires provider, source, account, request_id, operation and time")
		}
		for k, v := range e.Quantities {
			if !safeFields(k) {
				return fmt.Errorf("invalid quantity name")
			}
			if v < 0 {
				return fmt.Errorf("negative usage quantity")
			}
		}
		e.ID = EventID(*e)
	}
	for i := range s.Contexts {
		c := &s.Contexts[i]
		if !safeFields(c.Provider, c.AccountID, c.Source, c.SessionID, c.Model) {
			return fmt.Errorf("invalid context text")
		}
		if c.Provider == "" || c.AccountID == "" || c.Source == "" || c.SessionID == "" || c.ObservedAt.IsZero() {
			return fmt.Errorf("context requires provenance and session")
		}
		if (c.UsedTokens != nil && *c.UsedTokens < 0) || (c.LimitTokens != nil && *c.LimitTokens <= 0) || (c.UsedPercent != nil && (math.IsNaN(*c.UsedPercent) || math.IsInf(*c.UsedPercent, 0) || *c.UsedPercent < 0)) {
			return fmt.Errorf("invalid context occupancy")
		}
		var total int64
		estimated := false
		for _, p := range c.Components {
			if !safeFields(p.Name) {
				return fmt.Errorf("invalid context component name")
			}
			estimated = estimated || p.Estimated
			if p.Tokens < 0 || p.Tokens > math.MaxInt64-total {
				return fmt.Errorf("invalid context component")
			}
			total += p.Tokens
		}
		if c.UsedTokens != nil && total > *c.UsedTokens && !estimated {
			return fmt.Errorf("context components exceed observed occupancy")
		}
		c.ComponentDelta = nil
		if estimated && c.UsedTokens != nil {
			delta := total - *c.UsedTokens
			c.ComponentDelta = &delta
		}
	}
	for _, c := range s.Connections {
		if !safeFields(c.ID, c.Provider, c.AccountID, c.Kind, c.State, c.Error) {
			return fmt.Errorf("invalid connection text")
		}
	}
	return nil
}

// Merge retains the newest observation per identity. Quota percentages and
// costs from different scopes are deliberately not added together.
func Merge(ss ...Snapshot) Snapshot {
	out := Snapshot{Version: Version, GeneratedAt: time.Now().UTC()}
	qs := map[string]QuotaSnapshot{}
	cs := map[string]CostObservation{}
	es := map[string]UsageEvent{}
	xs := map[string]ContextSnapshot{}
	conns := map[string]ConnectionStatus{}
	warnings := map[string]bool{}
	for _, s := range ss {
		for _, q := range s.Quotas {
			if old, ok := qs[q.ID]; !ok || q.ObservedAt.After(old.ObservedAt) {
				qs[q.ID] = q
			}
		}
		for _, c := range s.Costs {
			if old, ok := cs[c.ID]; !ok || c.ObservedAt.After(old.ObservedAt) {
				cs[c.ID] = c
			}
		}
		for _, e := range s.Events {
			es[e.ID] = e
		}
		for _, x := range s.Contexts {
			k := contextID(x)
			if old, ok := xs[k]; !ok || x.ObservedAt.After(old.ObservedAt) {
				xs[k] = x
			}
		}
		for _, c := range s.Connections {
			k := stableID(c.Provider, c.AccountID, c.ID)
			old, ok := conns[k]
			if !ok || c.LastAttempt != nil && (old.LastAttempt == nil || c.LastAttempt.After(*old.LastAttempt)) {
				conns[k] = c
			}
		}
		for _, w := range s.Warnings {
			warnings[w] = true
		}
	}
	for _, q := range qs {
		out.Quotas = append(out.Quotas, q)
	}
	for _, c := range cs {
		out.Costs = append(out.Costs, c)
	}
	for _, e := range es {
		out.Events = append(out.Events, e)
	}
	for _, x := range xs {
		out.Contexts = append(out.Contexts, x)
	}
	for _, c := range conns {
		out.Connections = append(out.Connections, c)
	}
	for w := range warnings {
		out.Warnings = append(out.Warnings, w)
	}
	sort.Slice(out.Quotas, func(i, j int) bool { return out.Quotas[i].ID < out.Quotas[j].ID })
	sort.Slice(out.Costs, func(i, j int) bool { return out.Costs[i].ID < out.Costs[j].ID })
	sort.Slice(out.Events, func(i, j int) bool { return out.Events[i].ID < out.Events[j].ID })
	sort.Slice(out.Contexts, func(i, j int) bool { return contextID(out.Contexts[i]) < contextID(out.Contexts[j]) })
	sort.Slice(out.Connections, func(i, j int) bool { return out.Connections[i].ID < out.Connections[j].ID })
	sort.Strings(out.Warnings)
	return out
}

// EffectiveCosts gives reported cost precedence over an overlapping estimate
// within the same account/currency. Organization reports cover every scope;
// narrower reports cover only an identical scope. It suppresses overlapping
// observations rather than prorating bills or double counting partial periods.
func EffectiveCosts(costs []CostObservation) []CostObservation {
	order := make([]int, len(costs))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		a, b := &costs[order[i]], &costs[order[j]]
		if a.Kind != b.Kind {
			return a.Kind == "reported"
		}
		if costScopeLevel(a.Scope) != costScopeLevel(b.Scope) {
			return costScopeLevel(a.Scope) > costScopeLevel(b.Scope)
		}
		return a.ObservedAt.After(b.ObservedAt)
	})
	selected := make([]int, 0, len(costs))
	partial := make([]bool, 0, len(costs))
	groups := make(map[costAccountKey]*costOverlapIndex)
	for _, position := range order {
		c := costs[position]
		key := costAccountKey{c.Provider, c.AccountID, c.Currency}
		index := groups[key]
		if index == nil {
			index = &costOverlapIndex{}
			groups[key] = index
		}
		// Return the earliest accepted overlap, preserving the original greedy
		// precedence and exactly which observation receives the Partial flag.
		i := index.first(c, len(selected))
		if i < len(selected) {
			a := costs[selected[i]]
			if (a.Scope != "organization" && a.Scope != c.Scope) || c.StartAt.Before(a.StartAt) || c.EndAt.After(a.EndAt) {
				partial[i] = true
			}
			continue
		}
		index.add(c, len(selected))
		selected = append(selected, position)
		partial = append(partial, c.Partial)
	}
	out := make([]CostObservation, len(selected))
	for i, position := range selected {
		out[i] = costs[position]
		out[i].Partial = partial[i]
	}
	return out
}

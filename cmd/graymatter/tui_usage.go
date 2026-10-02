package main

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/usage"
)

type tuiUsageState struct {
	mode        string
	summaryMode string
	showContext bool
	showSources bool
	offset      int
	generation  uint64
	loading     bool
	loaded      bool
	stateDir    string
	sources     [2]usage.Snapshot
	errors      [2]string
	snapshot    usage.Snapshot
	legacy      harness.TokenUsageSummary
	legacyError string
	observedAt  time.Time
}

type tuiUsageLoadedMsg struct {
	generation uint64
	sources    [2]usage.Snapshot
	errors     [2]string
	legacy     harness.TokenUsageSummary
	legacyErr  string
	at         time.Time
	display    *usage.Snapshot
}

func (m *tuiModel) initUsage() {
	m.usage.mode = "auto"
	m.usage.summaryMode = "limits"
	m.usage.stateDir, _ = usage.DefaultStateDir()
}

func (m *tuiModel) loadUsage() tea.Cmd { return m.requestUsage(false) }

func (m *tuiModel) refreshUsage() tea.Cmd { return m.requestUsage(true) }

func (m *tuiModel) requestUsage(refresh bool) tea.Cmd {
	refresh = refresh && !m.demo
	if m.usage.loading {
		return nil
	}
	m.usage.loading = true
	m.usage.generation++
	generation, userDir, projectDir, store := m.usage.generation, m.usage.stateDir, m.dataDir, m.store
	previous := m.usage.sources
	parent := m.ctx
	if parent == nil {
		parent = context.Background()
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, 20*time.Second)
		defer cancel()
		result := tuiUsageLoadedMsg{generation: generation, at: time.Now().UTC()}
		if userDir == "" {
			var err error
			userDir, err = usage.DefaultStateDir()
			if err != nil {
				result.errors[0] = err.Error()
			}
		}
		for i, dir := range []string{userDir, projectDir} {
			if dir == "" {
				continue
			}
			// Account connections are configured only in user state. Project
			// state contributes local harness observations, never credentials.
			snapshot, err := usage.Load(ctx, dir, refresh && i == 0)
			result.sources[i] = snapshot
			if err != nil {
				result.errors[i] = err.Error()
			}
		}
		if store != nil {
			if err := ctx.Err(); err != nil {
				result.legacyErr = err.Error()
			} else {
				var err error
				result.legacy, err = store.TokenSummary(30)
				if err != nil {
					result.legacyErr = err.Error()
				}
			}
		}
		display := prepareUsageDisplay(result.sources, result.errors, previous)
		result.display = &display
		return result
	}
}

func (m *tuiModel) updateUsage(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case tuiUsageLoadedMsg:
		if msg.generation != m.usage.generation {
			return nil
		}
		m.usage.loading, m.usage.loaded = false, true
		for i := range msg.sources {
			m.usage.errors[i] = msg.errors[i]
			if msg.errors[i] == "" || msg.sources[i].Version == usage.Version {
				m.usage.sources[i] = msg.sources[i]
			} else {
				m.usage.sources[i] = staleUsageSnapshot(m.usage.sources[i])
			}
		}
		if msg.display != nil {
			m.usage.snapshot = *msg.display
		} else {
			m.usage.snapshot = prepareUsageDisplay(m.usage.sources, [2]string{}, [2]usage.Snapshot{})
		}
		m.usage.legacyError = msg.legacyErr
		if msg.legacyErr == "" {
			m.usage.legacy = msg.legacy
		}
		m.usage.observedAt = msg.at
	case tea.KeyPressMsg:
		m.usage.offset = min(m.usage.offset, m.usageMaxOffset())
		switch msg.String() {
		case "a":
			m.usage.mode, m.usage.offset = "auto", 0
		case "l":
			m.usage.mode, m.usage.offset = "limits", 0
		case "s":
			m.usage.mode, m.usage.offset = "spend", 0
		case "c":
			m.usage.showContext = !m.usage.showContext
			m.usage.offset = 0
		case "v":
			m.usage.showSources = !m.usage.showSources
		case "g":
			if m.usage.summaryMode == "spend" {
				m.usage.summaryMode = "limits"
			} else {
				m.usage.summaryMode = "spend"
			}
		case "r":
			return m.refreshUsage()
		case "up", "k":
			m.usage.offset = max(0, m.usage.offset-1)
		case "down", "j":
			m.usage.offset++
		case "pgup":
			m.usage.offset = max(0, m.usage.offset-max(1, m.height-10))
		case "pgdown", "space", " ":
			m.usage.offset += max(1, m.height-10)
		case "home":
			m.usage.offset = 0
		case "end":
			m.usage.offset = m.usageMaxOffset()
		}
	case tea.MouseWheelMsg:
		m.usage.offset = min(m.usage.offset, m.usageMaxOffset())
		if msg.Button == tea.MouseWheelUp {
			m.usage.offset = max(0, m.usage.offset-3)
		} else if msg.Button == tea.MouseWheelDown {
			m.usage.offset += 3
		}
	}
	m.usage.offset = min(m.usage.offset, m.usageMaxOffset())
	return nil
}

// Derive display data in the load command, not on each key or render. Imported
// ledgers can contain many observations even though the viewport is small.
func prepareUsageDisplay(sources [2]usage.Snapshot, failures [2]string, previous [2]usage.Snapshot) usage.Snapshot {
	for i := range sources {
		if failures[i] != "" && sources[i].Version != usage.Version {
			sources[i] = staleUsageSnapshot(previous[i])
		}
	}
	s := usage.Merge(sources[:]...)
	s.Costs = usage.EffectiveCosts(s.Costs)
	sort.SliceStable(s.Costs, func(i, j int) bool {
		if !s.Costs[i].StartAt.Equal(s.Costs[j].StartAt) {
			return s.Costs[i].StartAt.After(s.Costs[j].StartAt)
		}
		return s.Costs[i].ObservedAt.After(s.Costs[j].ObservedAt)
	})
	sort.SliceStable(s.Quotas, func(i, j int) bool {
		a, b := s.Quotas[i].UsedPercent, s.Quotas[j].UsedPercent
		return a != nil && (b == nil || *a > *b)
	})
	sort.Slice(s.Events, func(i, j int) bool { return s.Events[i].Time.After(s.Events[j].Time) })
	sort.Slice(s.Contexts, func(i, j int) bool { return s.Contexts[i].ObservedAt.After(s.Contexts[j].ObservedAt) })
	for _, group := range []struct {
		name         string
		count, limit int
	}{{"quota windows", len(s.Quotas), 100}, {"cost observations", len(s.Costs), 100}, {"session contexts", len(s.Contexts), 20}} {
		if group.count > group.limit {
			s.Warnings = append(s.Warnings, fmt.Sprintf("Showing %d of %d %s. Full records: graymatter usage show --json.", group.limit, group.count, group.name))
		}
	}
	s.Quotas = s.Quotas[:min(100, len(s.Quotas))]
	s.Costs = s.Costs[:min(100, len(s.Costs))]
	s.Contexts = s.Contexts[:min(20, len(s.Contexts))]
	return s
}

func (m tuiModel) usageMaxOffset() int {
	return max(0, len(m.usageLines(max(1, m.width-4)))-max(0, m.bodyHeight()-2))
}

func staleUsageSnapshot(s usage.Snapshot) usage.Snapshot {
	s.Quotas = append([]usage.QuotaSnapshot(nil), s.Quotas...)
	s.Costs = append([]usage.CostObservation(nil), s.Costs...)
	s.Contexts = append([]usage.ContextSnapshot(nil), s.Contexts...)
	s.Connections = append([]usage.ConnectionStatus(nil), s.Connections...)
	for i := range s.Quotas {
		s.Quotas[i].Stale = true
	}
	for i := range s.Costs {
		s.Costs[i].Stale = true
	}
	for i := range s.Contexts {
		s.Contexts[i].Stale = true
	}
	for i := range s.Connections {
		s.Connections[i].State = "stale"
	}
	return s
}

func (m tuiModel) usageSummary(width int) string {
	var text string
	s := m.usage.snapshot
	costs := s.Costs
	if m.usage.summaryMode != "spend" && len(s.Quotas) > 0 {
		q := s.Quotas[0]
		value := "—"
		if q.UsedPercent != nil {
			value = fmt.Sprintf("%.0f%% used", *q.UsedPercent)
		}
		text = value + " · " + q.Window + " · " + usageLabel(q.Provider, q.AccountID, q.Label)
		if q.Stale || q.Error != "" {
			text += " · stale"
		}
	} else if len(costs) > 0 {
		c := costs[0]
		amount := "—"
		if c.Amount != "" {
			amount = c.Amount + " " + strings.ToUpper(c.Currency)
		}
		text = amount + " · " + c.Kind + " · " + usageLabel(c.Provider, c.AccountID, "") + " · " + c.Scope
		if c.Partial {
			text += " · partial"
		}
		if c.Stale {
			text += " · stale"
		}
	} else if m.usage.legacy.Requests > 0 {
		text = fmt.Sprintf("harness ~%.4f USD · estimate · 30d UTC · partial", m.usage.legacy.TotalUSD)
		if m.usage.legacyError != "" {
			text += " · stale"
		}
	} else {
		text = "no usage source connected"
	}
	return fitCells(usageInk("u Usage", m.theme.Accent, true)+"  "+usageInk(text, m.theme.Muted, false), width, 1)
}

func usageLabel(provider, account, label string) string {
	if label == "" {
		label = provider
	}
	if account != "" {
		label += " / " + account
	}
	return label
}

func usageBar(percent *float64, width int) string {
	if width <= 0 {
		return ""
	}
	if percent == nil || math.IsNaN(*percent) || math.IsInf(*percent, 0) {
		return strings.Repeat("·", width)
	}
	used := int(math.Round(math.Max(0, math.Min(100, *percent)) * float64(width) / 100))
	return strings.Repeat("━", used) + strings.Repeat("─", width-used)
}

func usageReset(at *time.Time, now time.Time) string {
	if at == nil || at.IsZero() {
		return "reset unknown"
	}
	if !at.After(now) {
		return "reset passed · refresh"
	}
	return "reset in " + usageDuration(at.Sub(now))
}

func usageAge(at, now time.Time) string {
	if at.IsZero() {
		return "time unknown"
	}
	if at.After(now) {
		return "timestamp ahead"
	}
	if now.Sub(at) < time.Minute {
		return "just updated"
	}
	return usageDuration(now.Sub(at)) + " ago"
}

func usageDuration(d time.Duration) string {
	minutes := max(1, int(d.Round(time.Minute)/time.Minute))
	if minutes >= 24*60 {
		return fmt.Sprintf("%dd %dh", minutes/(24*60), (minutes/60)%24)
	}
	if minutes >= 60 {
		if minutes%60 == 0 {
			return fmt.Sprintf("%dh", minutes/60)
		}
		return fmt.Sprintf("%dh %dm", minutes/60, minutes%60)
	}
	return fmt.Sprintf("%dm", minutes)
}

func usagePeriod(start, end time.Time) string {
	if start.IsZero() || end.IsZero() {
		return "period unknown"
	}
	return start.UTC().Format("2006-01-02 15:04") + " – " + end.UTC().Format("2006-01-02 15:04") + " UTC"
}

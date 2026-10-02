package main

import (
	"fmt"
	"image/color"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// Data enters the layout through usageInk before any trusted terminal styles.
func usageInk(text string, ink color.Color, bold bool) string {
	return lipgloss.NewStyle().Foreground(ink).Bold(bold).Render(tuiPlain(text))
}

func usageColumn(text string, width int) string { return fitCells(text, width, 1) }

func usageNumber(n int64) string {
	s := fmt.Sprint(n)
	start := 0
	if strings.HasPrefix(s, "-") {
		start = 1
	}
	for i := len(s) - 3; i > start; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

func (m tuiModel) usageMeter(percent *float64, width int, ink color.Color) string {
	width = max(1, width)
	if percent == nil || math.IsNaN(*percent) || math.IsInf(*percent, 0) {
		return usageInk(strings.Repeat("·", width), m.theme.Border, false)
	}
	used := int(math.Round(math.Max(0, math.Min(100, *percent)) * float64(width) / 100))
	return usageInk(strings.Repeat("━", used), ink, false) + usageInk(strings.Repeat("─", width-used), m.theme.Border, false)
}

func (m tuiModel) renderUsage(width, height int) string {
	if width < 1 || height < 1 {
		return ""
	}
	inner := max(1, width-4)
	mode := m.usage.mode
	if mode == "" {
		mode = "auto"
	}
	var tabs []string
	for _, item := range []struct{ key, label string }{{"auto", "a Auto"}, {"limits", "l Limits"}, {"spend", "s Spend"}} {
		label := " " + item.label + " "
		if mode == item.key {
			label = "› " + item.label + " "
			label = lipgloss.NewStyle().Foreground(m.theme.Canvas).Background(m.theme.Accent).Bold(true).Render(label)
		} else {
			label = m.muted(label)
		}
		tabs = append(tabs, label)
	}
	header := usageInk("Usage", m.theme.Text, true) + "   " + strings.Join(tabs, " ")
	if m.usage.loading {
		header += m.muted("   refreshing…")
	}
	lines := m.usageLines(inner)
	available := max(0, height-2)
	offset := min(m.usage.offset, max(0, len(lines)-available))
	end := min(len(lines), offset+available)
	if len(lines) > available {
		position := m.muted(fmt.Sprintf("%d–%d / %d", offset+1, end, len(lines)))
		header += strings.Repeat(" ", max(1, inner-ansi.StringWidth(header)-ansi.StringWidth(position))) + position
	}
	body := ""
	if end > offset {
		body = strings.Join(lines[offset:end], "\n")
	}
	content := fitCells(header, inner, 1) + "\n" + strings.Repeat(" ", inner) + "\n" + fitCells(body, inner, available)
	rows := strings.Split(content, "\n")
	for i := range rows {
		rows[i] = "  " + rows[i] + "  "
	}
	return fitCells(strings.Join(rows, "\n"), width, height)
}

func (m tuiModel) usageLines(width int) []string {
	width = max(1, min(108, width))
	var lines []string
	styled := func(text string) { lines = append(lines, strings.Split(lipgloss.Wrap(text, width, ""), "\n")...) }
	text := func(s string) { styled(usageInk(s, m.theme.Text, false)) }
	note := func(s string) { styled(usageInk(s, m.theme.Muted, false)) }
	warn := func(s string) { styled(usageInk(s, m.theme.Warning, false)) }
	section := func(label string, ink color.Color) {
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		styled(usageInk(label, ink, true) + "  " + usageInk(strings.Repeat("─", max(0, width-ansi.StringWidth(label)-2)), m.theme.Border, false))
	}
	s, now := m.usage.snapshot, time.Now()
	mode := m.usage.mode
	if mode == "" {
		mode = "auto"
	}
	if !m.usage.loaded && !m.usage.loading {
		note("Press r to read configured sources.")
	}
	for i, err := range m.usage.errors {
		if err != "" {
			name := "Account sources"
			if i == 1 {
				name = "Project observations"
			}
			warn(name + ": unavailable · " + err)
		}
	}
	if mode != "spend" {
		section("Subscription limits", m.theme.Success)
		if len(s.Quotas) == 0 {
			note("No quota source connected. Configure graymatter usage.")
		}
		accountW := min(32, max(15, width/4))
		meterW := max(8, width-accountW-38)
		if width >= 78 && len(s.Quotas) > 0 {
			note(usageColumn("ACCOUNT", accountW) + "  " + usageColumn("WINDOW", 9) + "  " + usageColumn("USED", meterW+8) + "  RESET")
		}
		for _, q := range s.Quotas {
			label := q.Label
			if label == "" {
				label = q.Provider
			}
			value, state := "—", "reported"
			ink := m.theme.Success
			if q.UsedPercent == nil {
				state = "unavailable"
			} else if !math.IsNaN(*q.UsedPercent) && !math.IsInf(*q.UsedPercent, 0) {
				value = fmt.Sprintf("%.0f%%", *q.UsedPercent)
				if *q.UsedPercent >= 85 {
					ink = m.theme.Warning
				}
			}
			if q.Stale {
				state = "stale"
				ink = m.theme.Warning
			}
			if q.Error != "" {
				state = "unavailable"
				ink = m.theme.Warning
			}
			reset := strings.TrimPrefix(usageReset(q.ResetAt, now), "reset in ")
			if width >= 78 {
				styled(usageColumn(usageInk(label, m.theme.Text, true), accountW) + "  " + usageColumn(usageInk(q.Window, m.theme.Muted, false), 9) + "  " + m.usageMeter(q.UsedPercent, meterW, ink) + " " + usageInk(fmt.Sprintf("%6s", value), ink, true) + "   " + usageInk(reset, m.theme.Muted, false))
				if ansi.StringWidth(tuiPlain(label)) > accountW {
					note(label)
				}
			} else {
				styled(usageInk(label, m.theme.Text, true) + "  " + usageInk(value+" used", ink, true) + "  " + usageInk(q.Window, m.theme.Muted, false))
				styled(m.usageMeter(q.UsedPercent, min(28, max(4, width-24)), ink) + "  " + usageInk(reset, m.theme.Muted, false))
			}
			note(usageLabel(q.Provider, q.AccountID, "") + " · " + state + " · " + usageAge(q.ObservedAt, now))
			if width >= 78 && ansi.StringWidth(tuiPlain(q.Window)) > 9 {
				note("Window: " + q.Window)
			}
			if m.usage.showSources {
				note("Source: " + q.Source)
				if q.WindowMinutes != nil {
					note(fmt.Sprintf("Window duration: %d min", *q.WindowMinutes))
				}
			}
			if q.Error != "" {
				warn(q.Error)
			}
		}
	}
	if mode != "limits" {
		section("API spend", m.theme.Warning)
		if len(s.Costs) == 0 && m.usage.legacy.Requests == 0 {
			note("No spend recorded. Connect billing or import observations.")
		}
		if width >= 78 && len(s.Costs) > 0 {
			note(usageColumn("ACCOUNT", width-44) + "  " + fmt.Sprintf("%18s", "AMOUNT") + "  METHOD")
		}
		for _, c := range s.Costs {
			amount := "—"
			if c.Amount != "" {
				amount = c.Amount + " " + strings.ToUpper(c.Currency)
			}
			qualifier := c.Kind
			if c.Partial {
				qualifier += " · partial"
			}
			if c.Stale {
				qualifier += " · stale"
			}
			label := usageLabel(c.Provider, c.AccountID, "")
			if width >= 78 {
				styled(usageColumn(usageInk(label, m.theme.Text, true), width-44) + "  " + usageInk(fmt.Sprintf("%18s", amount), m.theme.Text, true) + "  " + usageInk(qualifier, m.theme.Warning, false))
				if ansi.StringWidth(tuiPlain(label)) > width-44 {
					note(label)
				}
			} else {
				styled(usageInk(label, m.theme.Text, true) + "  " + usageInk(amount, m.theme.Text, true))
				note(qualifier)
			}
			note(c.Scope + " · " + usagePeriod(c.StartAt, c.EndAt))
			if m.usage.showSources {
				note(c.Source + " · " + usageAge(c.ObservedAt, now))
			}
		}
		if m.usage.legacy.Requests > 0 {
			section("Legacy harness aggregate · separate, may overlap", m.theme.Warning)
			amount := fmt.Sprintf("~%.4f USD", m.usage.legacy.TotalUSD)
			if m.usage.legacy.Unpriced {
				amount += " known subtotal · unpriced usage excluded"
			}
			text(amount)
			note(fmt.Sprintf("%d requests · last 30 UTC calendar days · this store", m.usage.legacy.Requests))
			note("Estimated at current catalog rates. Not added to source costs.")
		}
		if m.usage.legacyError != "" {
			warn("Harness ledger unavailable: " + m.usage.legacyError)
		}
	}
	contextLabel := "Context"
	if m.usage.showContext {
		contextLabel += "  · c collapse"
	} else {
		contextLabel += "  · c expand"
	}
	section(contextLabel, m.theme.Info)
	if len(s.Contexts) == 0 {
		note("No session context observed. Files on disk are not live context.")
	}
	for _, c := range s.Contexts {
		method := "reported"
		if c.Estimated {
			method = "estimated"
		}
		if c.Stale {
			method += " · stale"
		}
		value := "—"
		if c.UsedTokens != nil {
			value = usageNumber(*c.UsedTokens)
		}
		if c.LimitTokens != nil {
			value += " / " + usageNumber(*c.LimitTokens)
		}
		value += " tokens"
		if c.UsedPercent != nil {
			value += fmt.Sprintf(" · %.1f%% used", *c.UsedPercent)
		}
		styled(usageInk(value, m.theme.Text, true) + "  " + usageInk(method, m.theme.Muted, false))
		note(usageLabel(c.Provider, c.AccountID, "") + " · session " + c.SessionID + " · " + usageAge(c.ObservedAt, now))
		if m.usage.showContext {
			remaining := ""
			if c.UsedTokens != nil && c.LimitTokens != nil && *c.LimitTokens >= *c.UsedTokens {
				remaining = usageNumber(*c.LimitTokens-*c.UsedTokens) + " tokens available"
			}
			styled(m.usageMeter(c.UsedPercent, max(8, width-29), m.theme.Info) + "  " + usageInk(remaining, m.theme.Muted, false))
			if len(c.Components) > 0 {
				lines = append(lines, "")
				nameW := min(36, max(12, width/3))
				note(usageColumn("COMPONENT", nameW) + "     TOKENS    SHARE OF USED CONTEXT")
			}
			for _, part := range c.Components {
				var percent *float64
				if c.UsedTokens != nil && *c.UsedTokens > 0 {
					p := float64(part.Tokens) * 100 / float64(*c.UsedTokens)
					percent = &p
				}
				quantity := usageNumber(part.Tokens)
				if part.Estimated {
					quantity = "~" + quantity
				}
				nameW := min(36, max(12, width/3))
				barW := max(4, width-nameW-21)
				share := "—"
				if percent != nil {
					share = fmt.Sprintf("%5.1f%%", *percent)
				}
				styled(usageColumn(usageInk(part.Name, m.theme.Text, false), nameW) + "  " + usageInk(fmt.Sprintf("%9s", quantity), m.theme.Text, false) + "  " + m.usageMeter(percent, barW, m.theme.Accent) + "  " + usageInk(share, m.theme.Muted, false))
				if ansi.StringWidth(tuiPlain(part.Name)) > nameW {
					note(part.Name)
				}
			}
			if len(c.Components) > 0 {
				note("~ estimated · component bars show share of observed used context")
			}
			if c.ComponentDelta != nil && *c.ComponentDelta != 0 {
				warn(fmt.Sprintf("Component sum differs from observed total by %+d tok; estimates retained.", *c.ComponentDelta))
			}
			if m.usage.showSources {
				note(c.Model + " · " + c.Source)
			}
		}
	}
	if mode != "limits" && len(s.Events) > 0 {
		section(fmt.Sprintf("Recent requests · %d records", len(s.Events)), m.theme.Accent)
		for _, event := range s.Events[:min(5, len(s.Events))] {
			text(event.Provider + " / " + event.AccountID + " · " + event.Model)
			note(event.Operation + " · " + event.Time.UTC().Format(time.RFC3339))
			if m.usage.showSources {
				keys := make([]string, 0, len(event.Quantities))
				for key := range event.Quantities {
					keys = append(keys, key)
				}
				sort.Strings(keys)
				var values []string
				for _, key := range keys {
					values = append(values, fmt.Sprintf("%s=%d", key, event.Quantities[key]))
				}
				note(strings.Join(values, " · "))
			}
		}
		if len(s.Events) > 5 {
			note("Showing 5 most recent events. Full records: usage show --json.")
		}
	}
	lines = append(lines, "")
	if m.usage.showSources {
		section("Sources & coverage · v collapse", m.theme.Muted)
		for _, c := range s.Connections {
			note(c.Provider + " / " + c.AccountID + " · " + c.Kind + " · " + c.State)
			if c.Error != "" {
				warn(c.Error)
			}
		}
		if len(s.Connections) == 0 {
			note("Use graymatter usage --help to connect or import a source.")
		}
		note("Account source settings: " + filepath.Join(m.usage.stateDir, "usage", "config.json"))
		note("— = unavailable. Quotas, currencies and overlapping costs are not summed.")
	} else {
		note("v Sources & coverage   ·   — unavailable   ·   each account and currency stays separate")
		// Failures remain visible even with provenance collapsed.
		for _, c := range s.Connections {
			if c.Error != "" {
				warn(c.Provider + " / " + c.AccountID + ": " + c.Error)
			}
		}
	}
	for _, warning := range s.Warnings {
		warn("Notice: " + warning)
	}
	return lines
}

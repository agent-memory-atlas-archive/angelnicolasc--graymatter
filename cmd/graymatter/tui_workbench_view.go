package main

import (
	"encoding/json"
	"fmt"
	"image/color"
	"os"
	"strings"
	"time"
	"unicode"

	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textarea"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/charmbracelet/x/ansi"
)

type tuiTheme struct{ Text, Muted, Accent, Border, Surface color.Color }

func (m *tuiModel) setTheme(name string) {
	if name == "" {
		name = "dark"
	}
	m.themeName = name
	switch name {
	case "light":
		m.theme = tuiTheme{lipgloss.Color("#20232E"), lipgloss.Color("#596273"), lipgloss.Color("#5743CA"), lipgloss.Color("#BFC3CD"), lipgloss.Color("#ECEAF8")}
	case "terminal":
		m.theme = tuiTheme{nil, nil, lipgloss.Color("5"), nil, nil}
	default:
		m.theme = tuiTheme{lipgloss.Color("#E7E8EF"), lipgloss.Color("#A0A7B6"), lipgloss.Color("#B2A2FF"), lipgloss.Color("#4B5265"), lipgloss.Color("#262538")}
	}
	m.input.SetStyles(textinput.DefaultStyles(name != "light"))
	m.editor.SetStyles(textarea.DefaultStyles(name != "light"))
}

// tuiPlain is the boundary between untrusted stored text and terminal control.
// ANSI, OSC hyperlinks and bidi override controls must never reach the renderer.
func tuiPlain(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, ansi.Strip(s))
}
func fitCells(s string, w, h int) string {
	if w <= 0 || h <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	out := make([]string, h)
	for i := range out {
		if i < len(lines) {
			out[i] = ansi.Truncate(lines[i], w, "")
		}
		out[i] += strings.Repeat(" ", max(0, w-ansi.StringWidth(out[i])))
	}
	return strings.Join(out, "\n")
}
func (m tuiModel) heading(s string) string {
	return lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).Render(s)
}
func (m tuiModel) muted(s string) string {
	return lipgloss.NewStyle().Foreground(m.theme.Muted).Render(s)
}
func (m tuiModel) panel(title, body string, w, h int, focused bool) string {
	if w < 3 || h < 4 {
		return fitCells(body, w, h)
	}
	c := m.theme.Border
	if focused {
		c = m.theme.Accent
	}
	marker := "  "
	if focused {
		marker = "› "
	}
	inside := m.heading(ansi.Truncate(marker+title, w-4, "…")) + "\n" + fitCells(body, w-4, h-3)
	return lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(c).Padding(0, 1).Render(fitCells(inside, w-4, h-2))
}
func (m tuiModel) columnWidths() (int, int, int) {
	if m.width < 100 {
		return 0, m.width, m.width
	}
	ns := 0
	if m.activeTab == tabMemory && m.width >= 120 {
		ns = min(26, m.width/5)
	}
	left := (m.width - ns) * 45 / 100
	return ns, left, m.width - ns - left
}
func (m *tuiModel) updateSizes() {
	if m.layoutWidth == m.width && m.layoutHeight == m.height && m.layoutTab == m.activeTab {
		return
	}
	m.layoutWidth, m.layoutHeight, m.layoutTab = m.width, m.height, m.activeTab
	h := max(1, m.height-10)
	ns, lw, dw := m.columnWidths()
	m.agentList.SetSize(max(1, ns-4), h)
	m.factList.SetSize(max(1, lw-4), h)
	m.sessionList.SetSize(max(1, lw-4), h)
	m.nodeList.SetSize(max(1, lw-4), h)
	m.recallList.SetSize(max(1, lw-4), h)
	m.checkpointList.SetSize(max(1, lw-4), h)
	if m.width < 100 {
		dw = m.width
	}
	m.detail.SetWidth(max(1, dw-4))
	m.detail.SetHeight(h)
	m.detail.SoftWrap = true
	if m.activeTab == tabStats {
		m.detail.SetWidth(max(1, m.width-4))
	}
	m.input.SetWidth(max(1, min(86, m.width-12)))
	m.editor.SetWidth(max(1, min(90, m.width-12)))
	m.editor.SetHeight(max(1, min(14, m.height-12)))
}
func (m tuiModel) View() tea.View {
	if m.width < 30 || m.height < 10 {
		v := tea.NewView(fitCells("GRAYMATTER\nResize to at least 30×10.\nq quit", m.width, m.height))
		v.AltScreen = true
		return v
	}
	bodyH := m.height - 7
	header := m.renderHeader()
	toolbar := m.renderToolbar()
	body := m.renderBody()
	footer := m.renderFooter()
	if m.modal != "" {
		body = m.renderModal(m.width, bodyH)
	} else if m.inputMode != "" {
		body = m.renderInput(m.width, bodyH)
	}
	content := header + "\n" + toolbar + "\n" + fitCells(body, m.width, bodyH) + "\n" + footer
	content = fitCells(content, m.width, m.height)
	if os.Getenv("NO_COLOR") != "" {
		content = ansi.Strip(content)
	}
	v := tea.NewView(content)
	v.AltScreen = true
	v.WindowTitle = "Graymatter · Memory Workbench"
	if m.mouse {
		v.MouseMode = tea.MouseModeCellMotion
	}
	return v
}
func (m tuiModel) renderHeader() string {
	brand := m.heading(" GRAYMATTER") + m.muted("  / memory workbench")
	right := m.muted("v" + version)
	if m.demo {
		right = m.heading("DEMO · sample data") + "  " + right
	}
	if m.readOnly {
		right = m.heading("READ ONLY") + "  " + right
	}
	gap := max(1, m.width-ansi.StringWidth(brand)-ansi.StringWidth(right)-1)
	var tabs []string
	for i, t := range tabNames {
		label := fmt.Sprintf(" %d %s ", i+1, t)
		if tabID(i) == m.activeTab {
			label = lipgloss.NewStyle().Bold(true).Foreground(m.theme.Accent).Background(m.theme.Surface).Render(label)
		} else {
			label = m.muted(label)
		}
		tabs = append(tabs, label)
	}
	return fitCells(brand+strings.Repeat(" ", gap)+right+"\n"+strings.Join(tabs, ""), m.width, 2)
}
func (m tuiModel) renderToolbar() string {
	name := m.namespace
	if name == "" {
		name = "choose namespace · n"
	}
	scope := " " + tuiPlain(name)
	switch m.activeTab {
	case tabMemory:
		scope += "  /  " + m.factMode
		if m.factMode == "" {
			scope += "active"
		}
		if m.factQuery != "" {
			scope += "  ·  filter: " + tuiPlain(m.factQuery)
		}
		scope += fmt.Sprintf("  ·  %d shown", len(m.factList.Items()))
		if m.factTotal > len(m.factList.Items()) {
			scope += fmt.Sprintf(" / %d  · Ctrl+N more", m.factTotal)
		}
	case tabRecall:
		scope += "  /  inspect retrieval  ·  Enter query"
	case tabSessions:
		if m.activityCheckpoints {
			scope += "  /  checkpoints"
		} else {
			scope = " Store-wide  /  harness runs"
		}
		scope += "  · c switch"
	case tabGraph:
		scope = fmt.Sprintf(" Store-wide graph  ·  %d entities  ·  %d edges  ·  %d orphans", len(m.nodeList.Items()), m.kgEdgeCount, m.kgOrphans)
	case tabUsage:
		scope = " Usage  /  source and account scope are shown with every observation"
	case tabStats:
		scope = " Status  /  observed store state  ·  provider network is not probed"
	}
	r := m.resources[m.activeResource()]
	state := ""
	if r.Loading {
		state = "Loading…"
	} else if r.Err != nil {
		state = "⚠ " + tuiPlain(r.Err.Error())
		if !r.Updated.IsZero() {
			state += " · showing last successful data"
		}
	} else if !r.Updated.IsZero() {
		state = "Updated " + r.Updated.Format("15:04:05")
	}
	if m.err != nil && m.activeTab == tabMemory {
		state = "⚠ " + tuiPlain(m.err.Error())
	}
	return fitCells(m.muted(scope)+"\n"+m.muted(" "+state), m.width, 2)
}
func (m tuiModel) activeResource() string {
	switch m.activeTab {
	case tabMemory:
		return "memory"
	case tabRecall:
		return "recall"
	case tabSessions:
		if m.activityCheckpoints {
			return "checkpoints"
		}
		return "activity"
	case tabGraph:
		return "graph"
	case tabStats:
		return "status"
	}
	return "usage"
}
func (m tuiModel) renderFooter() string {
	help := " ↑↓ navigate  Tab focus  / commands  1–6 tabs  r refresh  q quit"
	switch m.activeTab {
	case tabMemory:
		help = " f filter  n namespace  [ ] state  a add  e revise  p pin  d retire  y copy"
	case tabRecall:
		help = " Enter query  Tab inspect  y copy  / commands  1–6 tabs"
	case tabSessions:
		help = " c runs/checkpoints  Tab inspect  x stop run  / commands  1–6 tabs"
	case tabGraph:
		help = " o neighbors  s source facts  Tab inspect  / commands  1–6 tabs"
	case tabUsage:
		help = " a auto  l limits  s spend  c context  v sources  r refresh  ↑↓ scroll"
	}
	if m.modal == "add" || m.modal == "revise" {
		help = " Ctrl+S save  Enter new line  Esc cancel · input never executes navigation"
	} else if m.modal != "" {
		help = " Enter confirm  Esc cancel"
	} else if m.inputMode != "" {
		help = " Type to search  ↑↓ choose  Enter apply  Esc return"
	}
	status := m.status
	if status == "" {
		status = "/ commands · 1–6 tabs · r refresh · q quit"
	}
	glance := m.usageSummary(max(0, m.width-3))
	return fitCells(m.muted(help)+"\n "+tuiPlain(status)+"\n "+glance, m.width, 3)
}
func (m tuiModel) renderBody() string {
	h := m.height - 7
	switch m.activeTab {
	case tabMemory:
		return m.renderMemory(h)
	case tabRecall:
		return m.renderSplit("Results", m.recallList, "Retrieval receipt", h)
	case tabSessions:
		return m.renderSessions(h)
	case tabGraph:
		return m.renderGraph(h)
	case tabUsage:
		return m.renderUsage(m.width, h)
	case tabStats:
		return m.panel("System & corpus", m.detail.View(), m.width, h, true)
	}
	return ""
}

func (m tuiModel) renderList(l list.Model, w, h int, empty string) string {
	items := l.VisibleItems()
	if len(items) == 0 {
		return fitCells(m.muted(empty), w, h)
	}
	count := max(1, h/3)
	sel := l.Index()
	start := (sel / count) * count
	var rows []string
	for i := start; i < min(len(items), start+count); i++ {
		item, ok := items[i].(interface {
			Title() string
			Description() string
		})
		if !ok {
			continue
		}
		marker := "  "
		if i == sel {
			marker = "› "
		}
		title := ansi.Truncate(tuiPlain(item.Title()), max(1, w-2), "…")
		line := fitCells(marker+title, w, 1)
		if i == sel {
			line = lipgloss.NewStyle().Foreground(m.theme.Text).Background(m.theme.Surface).Bold(true).Render(line)
		}
		rows = append(rows, line, m.muted(ansi.Truncate("  "+tuiPlain(item.Description()), w, "…")), "")
	}
	return fitCells(strings.Join(rows, "\n"), w, h)
}
func (m tuiModel) renderMemory(h int) string {
	ns, lw, dw := m.columnWidths()
	facts := m.renderList(m.factList, lw-4, h-3, "No matching memories.\n\na add a fact · f change filter\n[ ] change lifecycle view")
	if m.width < 100 {
		if m.focus == 1 || m.memPane == memPaneDetail {
			return m.panel("Memory inspector · Esc returns", m.detail.View(), m.width, h, true)
		}
		if m.memPane == memPaneAgents {
			return m.panel("Namespaces · Enter opens", m.renderList(m.agentList, m.width-4, h-3, "No namespaces yet"), m.width, h, true)
		}
		return m.panel("Memory", facts, m.width, h, true)
	}
	parts := []string{}
	if ns > 0 {
		parts = append(parts, m.panel("Namespaces", m.renderList(m.agentList, ns-4, h-3, "No namespaces yet"), ns, h, m.memPane == memPaneAgents))
	}
	parts = append(parts, m.panel("Memory", facts, lw, h, m.memPane == memPaneFacts), m.panel("Inspector", m.detail.View(), dw, h, m.memPane == memPaneDetail))
	return lipgloss.JoinHorizontal(lipgloss.Top, parts...)
}
func (m tuiModel) renderSplit(title string, l list.Model, detailTitle string, h int) string {
	_, lw, dw := m.columnWidths()
	if m.width < 100 {
		if m.focus == 1 {
			return m.panel(detailTitle+" · Esc returns", m.detail.View(), m.width, h, true)
		}
		return m.panel(title, m.renderList(l, m.width-4, h-3, "No entries.\n\nUse the toolbar action or r to refresh."), m.width, h, true)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, m.panel(title, m.renderList(l, lw-4, h-3, "No entries yet."), lw, h, m.focus == 0), m.panel(detailTitle, m.detail.View(), dw, h, m.focus == 1))
}
func (m tuiModel) renderSessions(h int) string {
	if m.activityCheckpoints {
		return m.renderSplit("Checkpoints", m.checkpointList, "Checkpoint detail", h)
	}
	return m.renderSplit("Harness runs", m.sessionList, "Run detail", h)
}
func (m tuiModel) renderGraph(h int) string {
	if len(m.nodeList.Items()) == 0 {
		return m.panel("Knowledge graph", m.muted("No entities in this store.\n\nEnable extraction with graymatter init --kg,\nor explicitly link entities using memory_reflect.\n\nr refreshes this view."), m.width, h, false)
	}
	return m.renderSplit("Entities", m.nodeList, "Relationships & sources", h)
}

func (m *tuiModel) syncPreview(reset bool) {
	m.updateSizes()
	key := fmt.Sprintf("%d/%s/", m.activeTab, m.namespace)
	switch m.activeTab {
	case tabMemory:
		key += m.selectedFactID()
	case tabRecall:
		if r, ok := m.recallList.SelectedItem().(receiptItem); ok {
			key += r.receipt.Provenance.FactID
		}
	case tabSessions:
		if m.activityCheckpoints {
			if c, ok := m.checkpointList.SelectedItem().(checkpointItem); ok {
				key += c.cp.ID
			}
		} else if s, ok := m.sessionList.SelectedItem().(sessionItem); ok {
			key += s.s.ID
		}
	case tabGraph:
		if n, ok := m.nodeList.SelectedItem().(nodeItem); ok {
			key += n.n.ID
		}
	}
	if reset || key != m.previewID {
		m.detail.GotoTop()
	}
	m.previewID = key
	content := m.previewContent()
	if content != m.previewText {
		m.detail.SetContent(content)
		m.previewText = content
	}
}
func (m tuiModel) previewContent() string {
	switch m.activeTab {
	case tabMemory:
		if f, ok := m.factList.SelectedItem().(factItem); ok {
			return formatFactDetail(f.fact)
		}
		return "Select a memory to inspect its content and lifecycle.\n\nAdd a fact with a. Use n to choose a namespace."
	case tabRecall:
		if r, ok := m.recallList.SelectedItem().(receiptItem); ok {
			f := r.receipt
			rank := func(v int) string {
				if v == 0 {
					return "not ranked"
				}
				return fmt.Sprint(v)
			}
			return fmt.Sprintf("%s\n\nRETRIEVAL RECEIPT\nQuery: %s\nFact: %s\nWritten: %s\n\nVector rank: %s\nKeyword rank: %s\nRecency rank: %s\n%s\nRetention weight: %.4f · age %.1f days\n\nConfidence: %s\nPinned: %t\nReplaces: %s\n\nScores rank results; they are not probabilities.\nInspection does not update access counters.\nRanking may use your embedding provider.\nGraph enrichment is not included.", tuiPlain(f.Text), tuiPlain(m.recallQuery), tuiPlain(f.Provenance.FactID), f.Provenance.WrittenAt.Format(time.RFC3339), rank(f.Ranks.VectorRank), rank(f.Ranks.KeywordRank), rank(f.Ranks.RecencyRank), receiptScoreDetail(f), f.Weight, f.AgeDays, confidenceDetail(f.Provenance.Confidence), f.Provenance.Pinned, tuiPlain(strings.Join(f.Provenance.Supersedes, ", ")))
		}
		return "What would memory retrieve?\n\nPress Enter to enter a query.\n\nSafe inspection uses the existing ranking without access touches. The configured embedding provider may still receive the query.\n\nRRF is a ranking score, not confidence or probability."
	case tabSessions:
		if m.activityCheckpoints {
			if c, ok := m.checkpointList.SelectedItem().(checkpointItem); ok {
				b, _ := json.MarshalIndent(c.cp, "", "  ")
				return "CHECKPOINT\n" + tuiPlain(string(b))
			}
			return "No checkpoints for this namespace.\n\nCheckpoints describe recorded application state; external client conversations are not automatically captured."
		}
		if s, ok := m.sessionList.SelectedItem().(sessionItem); ok {
			r := s.s
			finish := "in progress"
			if r.FinishedAt != nil {
				finish = r.FinishedAt.Format(time.RFC3339)
			}
			return fmt.Sprintf("%s\n\nRun: %s\nNamespace: %s\nState: %s\nStarted: %s\nFinished: %s\nAttempts: %d\nPID: %d\nCheckpoint: %s\nLog file: %s\n\n%s\n\nCoverage: Graymatter harness runs only.\nc opens checkpoint history; x requests a confirmed stop.", tuiPlain(r.AgentFile), tuiPlain(r.ID), tuiPlain(r.AgentID), tuiPlain(r.Status), r.StartedAt.Format(time.RFC3339), finish, r.Attempts, r.PID, tuiPlain(r.LastCPID), tuiPlain(r.LogFile), tuiPlain(r.ErrorMsg))
		}
		return "No harness runs yet.\n\nRuns started by graymatter run appear here. This is not a list of all MCP client conversations."
	case tabGraph:
		if n, ok := m.nodeList.SelectedItem().(nodeItem); ok {
			var b strings.Builder
			b.WriteString(formatNodeDetail(n.n))
			b.WriteString("\nRELATIONSHIPS\n")
			count := 0
			for _, e := range m.graphEdges {
				if e.From != n.n.ID && e.To != n.n.ID {
					continue
				}
				count++
				b.WriteString(fmt.Sprintf("\n%s → %s\n  %s · weight %.3f\n", tuiPlain(m.nodeLabel(e.From)), tuiPlain(m.nodeLabel(e.To)), tuiPlain(e.Relation), e.Weight))
				if len(e.Sources) == 0 {
					b.WriteString("  source not recorded\n")
				} else {
					for _, id := range e.Sources {
						b.WriteString("  fact " + tuiPlain(id) + "\n")
					}
				}
			}
			if count == 0 {
				b.WriteString("No recorded neighbors.\n")
			}
			b.WriteString("\no browse neighbors · s open a supporting fact\nScope: store-wide; namespaces are not isolated here.\nSources are retained receipts, not a complete history.")
			return b.String()
		}
	case tabStats:
		return m.statusContent()
	}
	return ""
}
func (m tuiModel) nodeLabel(id string) string {
	for _, it := range m.nodeList.Items() {
		n := it.(nodeItem)
		if n.n.ID == id {
			return n.n.Label
		}
	}
	return id
}
func formatNodeDetail(n kg.Node) string {
	return fmt.Sprintf("Entity: %s\nType: %s\nID: %s\nWeight: %.4f\nFirst: %s\nLast: %s\n", tuiPlain(n.Label), tuiPlain(n.EntityType), tuiPlain(n.ID), n.Weight, n.FirstSeen.Format(time.RFC3339), n.LastSeen.Format(time.RFC3339))
}
func formatFactDetail(f memory.Fact) string {
	var b strings.Builder
	b.WriteString(tuiPlain(f.Text))
	b.WriteString("\n\nMEMORY DETAILS\n")
	b.WriteString(fmt.Sprintf("State: %s\nID: %s\nNamespace: %s\nCreated: %s\nLast accessed: %s\nAccesses: %d\nWeight: %.4f\nConfidence: %s\n", factState(f), tuiPlain(f.ID), tuiPlain(f.AgentID), f.CreatedAt.Format(time.RFC3339), f.AccessedAt.Format(time.RFC3339), f.AccessCount, f.Weight, confidenceDetail(f.Confidence)))
	if f.Pinned {
		b.WriteString("Pinned: yes (exempt from decay, pruning, summarisation)\n")
	}
	if f.IsAlias() {
		b.WriteString("Kind: vocabulary alias (not injectable)\nSource: " + tuiPlain(f.AliasSource) + "\n")
	}
	if f.IsSuperseded() {
		b.WriteString("Superseded by: " + tuiPlain(f.SupersededBy) + "\nThis record is excluded from retrieval.\n")
	}
	b.WriteString("\nFile/tool provenance: not recorded\nWeight is retention metadata, not probability of truth.\n")
	return b.String()
}

func receiptScoreDetail(f memory.RecallReceipt) string {
	out := fmt.Sprintf("Base RRF score: %.6f (k=%.0f)", f.Ranks.FusedScore, f.Ranks.K)
	if r := f.Ranking; r != nil {
		out += fmt.Sprintf("\nFinal score: %.6f\nConfidence factor: %.4f\nPolicy: %s · confidence weight %.4f\nEffective confidence: %s", r.FinalScore, r.Factor, tuiPlain(r.Policy), r.ConfidenceWeight, tuiPlain(r.EffectiveConfidence))
	}
	return out
}
func (m tuiModel) statusContent() string {
	d := m.dashboard
	if d.Err != nil {
		return "⚠ Store unreachable\n\n" + tuiPlain(d.Err.Error()) + "\n\nr retry · graymatter doctor for diagnostics"
	}
	if !d.Loaded {
		return "Loading store snapshot…"
	}
	var b strings.Builder
	b.WriteString("STORE SNAPSHOT\n\nDirectory: " + tuiPlain(m.dataDir) + "\n")
	b.WriteString(fmt.Sprintf("Read-only session: %t\nNamespaces: %d\nStored records: %d (all lifecycle states)\nPayload estimate: %s (text + embeddings, not disk size)\nFact accesses: %s (not query count)\nMean retention weight: %.3f\n\n", m.readOnly, d.AgentsN, d.FactsN, formatBytes(d.StorageB), formatCompact(d.RecallsN), d.AvgWeight))
	if d.FactsN == 0 {
		b.WriteString("No memories stored yet. Add one from Memory.\n\n")
	}
	b.WriteString("RESOURCE FRESHNESS\n")
	if r := m.resources["provider"]; !r.Updated.IsZero() {
		h := m.health
		b.WriteString(fmt.Sprintf("Provider: %s · configured: %t · reachability: %s\nEmbedding dimensions: %d · degraded writes: %d · pending vectors: %d\n", tuiPlain(h.Provider), h.ProviderConfigured, tuiPlain(h.ProviderReachability), h.Embedding.EmbedDims, h.Embedding.DegradedFacts, h.Embedding.PendingVectors))
		if h.Embedding.LastDegradError != "" {
			b.WriteString("Last degradation: " + tuiPlain(h.Embedding.LastDegradError) + "\n")
		}
		b.WriteString("\n")
	}
	for _, name := range []string{"namespaces", "memory", "recall", "activity", "checkpoints", "graph", "status", "provider"} {
		r := m.resources[name]
		state := "not loaded"
		if !r.Updated.IsZero() {
			state = "last success " + r.Updated.Format(time.RFC3339)
		}
		if r.Loading {
			state += " · loading"
		}
		if r.Err != nil {
			state += " · error: " + tuiPlain(r.Err.Error())
		}
		b.WriteString(name + ": " + state + "\n")
	}
	b.WriteString("\nCorpus activity counts fact creation, not retrieval requests.\nAPI spend and subscriptions are in Usage (u).\nProvider connectivity is not inferred from database connectivity.\n")
	return b.String()
}

func (m tuiModel) renderInput(w, h int) string {
	title := map[string]string{"palette": "Commands", "filter": "Filter current list", "namespace": "Choose namespace", "recall": "Inspect recall", "graph-neighbor": "Connected entities", "graph-source": "Supporting facts"}[m.inputMode]
	var lines []string
	lines = append(lines, m.input.View(), "")
	limit := max(1, h-7)
	selected := m.paletteIndex
	switch m.inputMode {
	case "palette":
		cs := m.matchingCommands()
		if len(cs) > 0 {
			selected = min(selected, len(cs)-1)
		}
		start := max(0, selected-limit+1)
		for i := start; i < min(len(cs), start+limit); i++ {
			c := cs[i]
			mark := "  "
			if i == selected {
				mark = "› "
			}
			lines = append(lines, mark+"/"+c.name+"  "+m.muted(c.description)+"  "+c.key)
		}
	case "namespace":
		ns := m.namespaceMatches(m.input.Value())
		if len(ns) == 0 && !m.readOnly {
			lines = append(lines, "Enter selects a new namespace; a adds its first fact.")
		}
		if len(ns) > 0 {
			selected = min(selected, len(ns)-1)
		}
		start := max(0, selected-limit+1)
		for i := start; i < min(len(ns), start+limit); i++ {
			mark := "  "
			if i == selected {
				mark = "› "
			}
			lines = append(lines, mark+tuiPlain(ns[i].id)+fmt.Sprintf("  %d records", ns[i].count))
		}
	case "graph-neighbor":
		ns := m.neighborMatches(m.input.Value())
		if len(ns) > 0 {
			selected = min(selected, len(ns)-1)
		}
		start := max(0, selected-limit+1)
		for i := start; i < min(len(ns), start+limit); i++ {
			mark := "  "
			if i == selected {
				mark = "› "
			}
			lines = append(lines, mark+tuiPlain(ns[i].n.Label))
		}
	case "graph-source":
		ids := m.sourceMatches(m.input.Value())
		if len(ids) > 0 {
			selected = min(selected, len(ids)-1)
		}
		start := max(0, selected-limit+1)
		for i := start; i < min(len(ids), start+limit); i++ {
			mark := "  "
			if i == selected {
				mark = "› "
			}
			lines = append(lines, mark+tuiPlain(ids[i]))
		}
	case "recall":
		lines = append(lines, "Uses the selected namespace: "+tuiPlain(m.namespace), "No access touches, alias learning or recall hooks.", "Your configured embedding provider may process the query.", "Enter submits · Esc returns without running a query.")
	case "filter":
		lines = append(lines, "Case-insensitive text filter, not semantic retrieval.", "Enter applies · empty text clears the filter.")
	}
	return m.panel(title, strings.Join(lines, "\n"), w, h, true)
}
func (m tuiModel) renderModal(w, h int) string {
	title, body := "", ""
	switch m.modal {
	case "add":
		title = "Add memory · " + m.modalAgent
		body = m.editor.View() + "\n\nCtrl+S save · Esc cancel"
	case "revise":
		title = "Revise memory · " + shortID(m.modalFact.ID)
		body = m.editor.View() + "\n\nCreates a replacement; preserves the original as history.\nCtrl+S save · Esc cancel"
	case "retire":
		title = "Retire this memory?"
		lines := strings.Split(ansi.Hardwrap(tuiPlain(m.modalFact.Text), max(1, w-6), false), "\n")
		limit := max(1, h-10)
		if len(lines) > limit {
			lines = append(lines[:limit], "… (excerpt; full text remains in the inspector)")
		}
		body = "Namespace: " + tuiPlain(m.modalFact.AgentID) + "\nID: " + tuiPlain(m.modalFact.ID) + "\n\n" + strings.Join(lines, "\n") + "\n\nIt stops appearing in recall and remains in history.\nEnter confirms · Esc cancels"
	case "kill":
		title = "Stop this running harness session?"
		body = tuiPlain(m.modalSession) + "\n\nThe session's process will be terminated.\nEnter confirms · Esc cancels"
	case "help":
		title = "Keyboard & scope"
		body = "1–6 views · / or Ctrl+K commands · q quit\nTab / Shift+Tab focus · ↑↓ or j/k navigate\nEnter inspect · Esc return · y copy · Ctrl+L redraw\nMemory: n namespace · f filter · [ ] lifecycle\na add · e revise · p pin/unpin · d retire · Ctrl+N more\nRecall: Enter query (safe inspection)\nActivity: c runs/checkpoints · x confirmed stop\nGraph: o neighbors · s supporting facts\nUsage: a auto · l limits · s spend · c context\nGraph is store-wide; Activity covers harness runs.\nNO_COLOR or --theme light|dark|terminal\nOptional --mouse; copy falls back to OSC52.\nEsc returns to your current selection."
	}
	if m.busy {
		body += "\n\nSaving…"
	}
	return m.panel(title, body, w, h, true)
}

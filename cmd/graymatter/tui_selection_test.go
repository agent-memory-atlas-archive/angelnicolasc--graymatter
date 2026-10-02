package main

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/harness"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/kg"
	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/session"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// Run returned load/mutation commands through Update so the regression covers
// the same asynchronous result messages as the interactive add/filter flow.
func drainSelectionCommand(t *testing.T, m tuiModel, cmd tea.Cmd) tuiModel {
	t.Helper()
	if cmd == nil {
		return m
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			m = drainSelectionCommand(t, m, c)
		}
		return m
	}
	next, follow := m.Update(msg)
	return drainSelectionCommand(t, next.(tuiModel), follow)
}

func TestWorkbenchAddThenFilterSelectsSingleAsyncResult(t *testing.T) {
	store, err := openDemoStore(context.Background(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for i := 0; i < 5; i++ {
		if err := store.Remember(context.Background(), "project", fmt.Sprintf("Existing record %d", i)); err != nil {
			t.Fatal(err)
		}
	}
	m := newTUIModel(store, t.TempDir(), false, "dark", false)
	m.namespace = "project"
	m = drainSelectionCommand(t, m, m.loadFacts(m.namespace))
	m.factList.Select(3)
	previousID := m.selectedFactID()
	m, _ = press(m, keyMsg('a'))
	const text = "Release checks run before every production deployment."
	m.editor.SetValue(text)
	m, save := press(m, tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	m = drainSelectionCommand(t, m, save)
	if m.modal != "" || m.selectedFactID() != previousID {
		t.Fatal("add reload lost the existing selected identity")
	}
	m, _ = press(m, keyMsg('f'))
	m.input.SetValue("Existing")
	m, keep := press(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = drainSelectionCommand(t, m, keep)
	if m.selectedFactID() != previousID {
		t.Fatal("filter lost an identity that is still in its results")
	}
	m, _ = press(m, keyMsg('f'))
	m.input.SetValue("Release checks")
	m, filter := press(m, tea.KeyPressMsg{Code: tea.KeyEnter})
	m = drainSelectionCommand(t, m, filter)
	if m.factTotal != 1 || m.factList.Index() != 0 || m.selectedText() != text {
		t.Fatalf("filter selection: total=%d index=%d text=%q", m.factTotal, m.factList.Index(), m.selectedText())
	}
	if !strings.Contains(m.previewContent(), text) || strings.Contains(m.View().Content, "Memory · 4/1") {
		t.Fatal("inspector or selection count did not follow the filtered result")
	}
	selectedID := m.selectedFactID()
	m, pin := press(m, keyMsg('p'))
	if pin == nil {
		t.Fatal("filtered fact cannot be pinned")
	}
	m = drainSelectionCommand(t, m, pin)
	selected, ok := m.factList.SelectedItem().(factItem)
	if !ok || !selected.fact.Pinned || selected.fact.ID != selectedID {
		t.Fatal("pin changed or lost the filtered selection")
	}
}

func TestWorkbenchListRefreshPreservesIdentityOrSelectsFirst(t *testing.T) {
	m := workbenchModel(t, nil)
	m.activeTab = tabGraph
	items := []list.Item{nodeItem{kg.Node{ID: "a", Label: "Match A"}}, nodeItem{kg.Node{ID: "b", Label: "Match B"}}, nodeItem{kg.Node{ID: "c", Label: "Other C"}}, nodeItem{kg.Node{ID: "d", Label: "Other D"}}}
	m.nodeList.SetItems(items)
	m.nodeList.Select(3)
	setListItems(&m.nodeList, items[:1])
	if m.nodeList.Index() != 0 || m.nodeList.SelectedItem() == nil {
		t.Fatal("shrinking list retained an invalid cursor")
	}
	setListItems(&m.nodeList, items)
	m.nodeList.Select(1)
	setListItems(&m.nodeList, []list.Item{items[1], items[0]})
	if n := m.nodeList.SelectedItem().(nodeItem); n.n.ID != "b" {
		t.Fatal("refresh preserved position instead of identity")
	}
	m.filterLocal("")
	setListItems(&m.nodeList, []list.Item{items[0]})
	if len(m.nodeList.VisibleItems()) != 1 || m.nodeList.SelectedItem() == nil {
		t.Fatal("empty applied filter left asynchronous matches unresolved")
	}
}

func TestWorkbenchAsyncListsClampShrinkingResults(t *testing.T) {
	cases := []struct {
		name string
		get  func(*tuiModel) *list.Model
		item func(string) list.Item
		msg  tea.Msg
	}{
		{"namespaces", func(m *tuiModel) *list.Model { return &m.agentList }, func(id string) list.Item { return agentItem{id: id} }, agentsLoadedMsg{agents: []agentItem{{id: "only"}}}},
		{"memory", func(m *tuiModel) *list.Model { return &m.factList }, func(id string) list.Item { return factItem{memory.Fact{ID: id}} }, factsLoadedMsg{agent: "project", facts: []factItem{{memory.Fact{ID: "only"}}}, total: 1}},
		{"recall", func(m *tuiModel) *list.Model { return &m.recallList }, func(id string) list.Item {
			return receiptItem{receipt: memory.RecallReceipt{Provenance: memory.RecallProvenance{FactID: id}}}
		}, recallLoadedMsg{agent: "project", receipts: []memory.RecallReceipt{{Provenance: memory.RecallProvenance{FactID: "only"}}}}},
		{"activity", func(m *tuiModel) *list.Model { return &m.sessionList }, func(id string) list.Item { return sessionItem{harness.HarnessSession{ID: id}} }, sessionsLoadedMsg{sessions: []sessionItem{{harness.HarnessSession{ID: "only"}}}}},
		{"graph", func(m *tuiModel) *list.Model { return &m.nodeList }, func(id string) list.Item { return nodeItem{kg.Node{ID: id}} }, nodesLoadedMsg{nodes: []nodeItem{{kg.Node{ID: "only"}}}}},
		{"checkpoints", func(m *tuiModel) *list.Model { return &m.checkpointList }, func(id string) list.Item { return checkpointItem{session.Checkpoint{ID: id}} }, checkpointsLoadedMsg{agent: "project", checkpoints: []session.Checkpoint{{ID: "only"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := workbenchModel(t, nil)
			l := tc.get(&m)
			l.SetItems([]list.Item{tc.item("a"), tc.item("b"), tc.item("c"), tc.item("d")})
			l.Select(3)
			next, _ := m.Update(tc.msg)
			m = next.(tuiModel)
			l = tc.get(&m)
			if l.Index() != 0 || listIdentity(l.SelectedItem()) != "only" {
				t.Fatalf("stale cursor: index%d selected%q", l.Index(), listIdentity(l.SelectedItem()))
			}
		})
	}
}

func TestWorkbenchSourceFactResetsSelectionAndClearsLayout(t *testing.T) {
	m := workbenchModel(t, nil)
	m.activeTab = tabGraph
	m.factList.SetItems([]list.Item{factItem{memory.Fact{ID: "a"}}, factItem{memory.Fact{ID: "b"}}, factItem{memory.Fact{ID: "c"}}, factItem{memory.Fact{ID: "d"}}})
	m.factList.Select(3)
	source := memory.Fact{ID: "source", AgentID: "project", Text: "The supporting fact."}
	next, cmd := m.Update(sourceFactMsg{fact: source})
	m = next.(tuiModel)
	if m.activeTab != tabMemory || m.focus != 1 || m.factList.Index() != 0 || m.selectedFactID() != source.ID || !strings.Contains(m.previewContent(), source.Text) {
		t.Fatal("source inspection inherited the previous memory cursor")
	}
	if cmd == nil || reflect.TypeOf(cmd()) != reflect.TypeOf(tea.ClearScreen()) {
		t.Fatal("cross-view source inspection did not clear the old layout")
	}
}

func TestWorkbenchFilteredListPaginationMatchesVisibleRows(t *testing.T) {
	for _, name := range []string{"graph", "runs", "checkpoints"} {
		t.Run(name, func(t *testing.T) {
			m := workbenchModel(t, nil)
			m.width, m.height = 80, 24
			m.activeTab = tabGraph
			if name != "graph" {
				m.activeTab = tabSessions
				m.activityCheckpoints = name == "checkpoints"
			}
			var items []list.Item
			for i := 0; i < 20; i++ {
				id := fmt.Sprintf("item-%02d", i)
				switch name {
				case "graph":
					items = append(items, nodeItem{kg.Node{ID: id, Label: id}})
				case "runs":
					items = append(items, sessionItem{harness.HarnessSession{ID: id, AgentFile: id}})
				case "checkpoints":
					items = append(items, checkpointItem{session.Checkpoint{ID: id}})
				}
			}
			setListItems(m.activeList(), items)
			m.updateSizes()
			for _, query := range []string{"item", ""} {
				m.activeList().Select(0)
				m.filterLocal(query)
				if got := m.activeList().Paginator.PerPage; got != 8 {
					t.Fatalf("filter %q: keyboard fits %d items; visible area fits 8", query, got)
				}
				m, _ = press(m, tea.KeyPressMsg{Code: tea.KeyPgDown})
				if got := listIdentity(m.activeList().SelectedItem()); got != "item-08" {
					t.Fatalf("PageDown selected %q", got)
				}
				m.handleMouse(tea.MouseClickMsg{X: 2, Y: 8, Button: tea.MouseLeft})
				if got := listIdentity(m.activeList().SelectedItem()); got != "item-09" {
					t.Fatalf("second visible row selected %q", got)
				}
			}
		})
	}
}

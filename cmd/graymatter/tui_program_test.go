package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	graymatter "github.com/angelnicolasc/graymatter"
)

type workbenchProbe struct{}
type workbenchProbeState struct {
	modal, status, text, id string
	busy, loading           bool
	focus                   int
	pane                    memPane
	width, height           int
}

// Exercise the actual Bubble Tea event loop and renderer, not just Update.
// Output deliberately uses an in-memory sink: this isolates model/command
// routing from ConPTY, ttyd, XON/XOFF and native-terminal transport behavior.
func TestWorkbenchProgramResizeRevisionAndQuit(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "VOYAGE_API_KEY"} {
		t.Setenv(key, "")
	}
	cfg := graymatter.DefaultConfig()
	cfg.DataDir = t.TempDir()
	cfg.EmbeddingMode = graymatter.EmbeddingKeyword
	cfg.ConsolidateLLM = ""
	cfg.AsyncConsolidate = false
	mem, err := graymatter.NewWithConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mem.Close()
	store := &directStore{mem: mem, store: mem.Advanced()}
	if err := store.Remember(context.Background(), "test-project", "The selected fact is safe to revise."); err != nil {
		t.Fatal(err)
	}
	m := newTUIModel(store, cfg.DataDir, false, "dark", false)
	m.namespace = "test-project"
	msg := m.loadFacts(m.namespace)()
	loaded, _ := m.Update(msg)
	m = loaded.(tuiModel)
	initialID := m.selectedFactID()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	m.ctx = ctx
	m.startup = func() tea.Msg { return statusMsg{"integration ready"} }
	observations := make(chan workbenchProbeState, 1)
	p := tea.NewProgram(m, tea.WithContext(ctx), tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithWindowSize(119, 36), tea.WithoutSignalHandler(), tea.WithFilter(func(model tea.Model, msg tea.Msg) tea.Msg {
		if _, ok := msg.(workbenchProbe); ok {
			m := model.(tuiModel)
			observations <- workbenchProbeState{m.modal, m.status, m.selectedText(), m.selectedFactID(), m.busy, m.resources["memory"].Loading, m.focus, m.memPane, m.width, m.height}
			return nil
		}
		return msg
	}))
	done := make(chan error, 1)
	go func() { _, err := p.Run(); done <- err }()
	defer p.Kill()
	send := func(msg tea.Msg) {
		t.Helper()
		sent := make(chan struct{})
		go func() { p.Send(msg); close(sent) }()
		select {
		case <-sent:
		case <-ctx.Done():
			t.Fatal("event loop stopped accepting messages")
		}
	}
	await := func(label string, predicate func(workbenchProbeState) bool) workbenchProbeState {
		t.Helper()
		var last workbenchProbeState
		for {
			send(workbenchProbe{})
			select {
			case last = <-observations:
				if predicate(last) {
					return last
				}
			case <-ctx.Done():
				t.Fatalf("%s: timed out; last state %+v", label, last)
			}
			select {
			case <-time.After(5 * time.Millisecond):
			case <-ctx.Done():
				t.Fatalf("%s: timed out; last state %+v", label, last)
			}
		}
	}
	await("startup", func(s workbenchProbeState) bool { return s.status == "integration ready" })
	send(keyMsg('e'))
	await("editor", func(s workbenchProbeState) bool { return s.modal == "revise" })
	send(tea.WindowSizeMsg{Width: 80, Height: 23})
	await("resize", func(s workbenchProbeState) bool { return s.width == 80 && s.height == 23 && s.modal == "revise" })
	send(tea.PasteMsg{Content: " Integration edit."})
	send(tea.KeyPressMsg{Code: 's', Mod: tea.ModCtrl})
	await("confirmed revision and async reload", func(s workbenchProbeState) bool {
		return strings.Contains(s.status, "confirmed") && !s.busy && !s.loading && s.modal == "" && s.id != initialID && strings.Contains(s.text, "Integration edit")
	})
	send(tea.KeyPressMsg{Code: tea.KeyEnter})
	await("inspector", func(s workbenchProbeState) bool { return s.focus == 1 && s.pane == memPaneDetail })
	send(tea.KeyPressMsg{Code: tea.KeyEsc})
	await("escape", func(s workbenchProbeState) bool { return s.focus == 0 && s.pane == memPaneFacts })
	send(keyMsg('d'))
	await("retire confirmation", func(s workbenchProbeState) bool { return s.modal == "retire" })
	send(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("Ctrl+C failed to terminate Program")
	}
}

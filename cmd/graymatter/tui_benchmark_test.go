package main

import (
	"charm.land/bubbles/v2/list"
	"fmt"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"os"
	"runtime"
	"sort"
	"testing"
	"time"
)

// These measurements isolate rendering/navigation over the loaded 100-item
// window. CorpusTotal changes only the truthful pagination label; backend
// latency and loading 100k actual rows are deliberately measured separately.
func benchmarkWorkbench(width, height, total int) tuiModel {
	m := newTUIModel(nil, "", true, "dark", false)
	m.width, m.height, m.factTotal = width, height, total
	m.namespace = "benchmark"
	items := make([]list.Item, 100)
	for i := range items {
		items[i] = factItem{memory.Fact{ID: fmt.Sprintf("fact-%04d", i), AgentID: "benchmark", Text: fmt.Sprintf("Memory %d: retain this project convention with provenance and lifecycle metadata.", i), CreatedAt: time.Unix(1700000000, 0), Weight: 1}}
	}
	m.factList.SetItems(items)
	m.syncPreview(true)
	return m
}

func BenchmarkWorkbenchRender100(b *testing.B) {
	for _, size := range [][2]int{{80, 24}, {120, 36}, {160, 48}} {
		for _, total := range []int{100, 10000, 100000} {
			b.Run(fmt.Sprintf("%dx%d/corpus_label_%d", size[0], size[1], total), func(b *testing.B) {
				m := benchmarkWorkbench(size[0], size[1], total)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					runtime.KeepAlive(m.View().Content)
				}
			})
		}
	}
}

func BenchmarkWorkbenchNavigate100(b *testing.B) {
	m := benchmarkWorkbench(120, 36, 100000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := 'j'
		if i%2 == 1 {
			key = 'k'
		}
		m, _ = press(m, keyMsg(key))
	}
	runtime.KeepAlive(m)
}

func TestWorkbenchLatencyProfile(t *testing.T) {
	if os.Getenv("GM_TUI_PROFILE") == "" {
		t.Skip("set GM_TUI_PROFILE to measure local render and navigation latency")
	}
	m := benchmarkWorkbench(120, 36, 100000)
	for _, name := range []string{"render", "navigate"} {
		samples := make([]time.Duration, 250)
		for i := range samples {
			start := time.Now()
			if name == "render" {
				runtime.KeepAlive(m.View().Content)
			} else {
				key := 'j'
				if i%2 == 1 {
					key = 'k'
				}
				m, _ = press(m, keyMsg(key))
			}
			samples[i] = time.Since(start)
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		t.Logf("%s 120x36 page=100 corpus_label=100000 samples=%d p50=%s p95=%s", name, len(samples), samples[len(samples)/2], samples[len(samples)*95/100])
	}
}

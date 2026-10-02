package usage

import (
	"fmt"
	"math/rand"
	"reflect"
	"sort"
	"testing"
	"time"
)

// Keep the original quadratic selector as an independent behavioral oracle:
// ordering, the first winning overlap and Partial propagation are contractual.
func referenceEffectiveCosts(costs []CostObservation) []CostObservation {
	xs := append([]CostObservation(nil), costs...)
	sort.SliceStable(xs, func(i, j int) bool {
		a, b := xs[i], xs[j]
		if a.Kind != b.Kind {
			return a.Kind == "reported"
		}
		if costScopeLevel(a.Scope) != costScopeLevel(b.Scope) {
			return costScopeLevel(a.Scope) > costScopeLevel(b.Scope)
		}
		return a.ObservedAt.After(b.ObservedAt)
	})
	out := []CostObservation{}
	for _, c := range xs {
		skip := false
		for i, a := range out {
			if a.Provider != c.Provider || a.AccountID != c.AccountID || a.Currency != c.Currency || !a.StartAt.Before(c.EndAt) || !c.StartAt.Before(a.EndAt) {
				continue
			}
			disjoint := a.Scope != c.Scope && costScopeLevel(a.Scope) > 0 && costScopeLevel(a.Scope) == costScopeLevel(c.Scope)
			if !disjoint {
				if (a.Scope != "organization" && a.Scope != c.Scope) || c.StartAt.Before(a.StartAt) || c.EndAt.After(a.EndAt) {
					out[i].Partial = true
				}
				skip = true
				break
			}
		}
		if !skip {
			out = append(out, c)
		}
	}
	return out
}

func TestEffectiveCostsIndexedParity(t *testing.T) {
	rng := rand.New(rand.NewSource(1701))
	at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	scopes := []string{"organization", "project:a", "project:b", "session:a", "session:b", "request:a", "request:b", "unknown-a", "unknown-b", ""}
	for iteration := 0; iteration < 70; iteration++ {
		costs := make([]CostObservation, 450)
		for i := range costs {
			start := at.Add(time.Duration(rng.Intn(150)) * time.Hour)
			costs[i] = CostObservation{ID: fmt.Sprint(i), Provider: fmt.Sprint(rng.Intn(2)), AccountID: fmt.Sprint(rng.Intn(3)), Currency: []string{"USD", "EUR"}[rng.Intn(2)], Kind: []string{"reported", "estimate"}[rng.Intn(2)], Scope: scopes[rng.Intn(len(scopes))], StartAt: start, EndAt: start.Add(time.Duration(1+rng.Intn(20)) * time.Hour), ObservedAt: at.Add(time.Duration(rng.Intn(5)) * time.Hour), Amount: "0.001", Partial: rng.Intn(10) == 0}
		}
		original := append([]CostObservation(nil), costs...)
		want, got := referenceEffectiveCosts(costs), EffectiveCosts(costs)
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("iteration %d: indexed selector changed greedy ordering/partial flags\nwant=%+v\ngot=%+v", iteration, want, got)
		}
		if !reflect.DeepEqual(original, costs) {
			t.Fatal("mutated caller ledger")
		}
	}
}

func TestEffectiveCostsKeepsFirstAcceptedOverlap(t *testing.T) {
	at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	base := CostObservation{Provider: "provider", AccountID: "account", Currency: "USD", Kind: "reported", Scope: "request:one", Amount: ".01", StartAt: at, EndAt: at.Add(time.Hour), ObservedAt: at}
	one, two, estimate := base, base, base
	one.ID = "first"
	two.ID = "second"
	two.Scope = "request:two"
	estimate.ID = "estimate"
	estimate.Kind = "estimate"
	estimate.Scope = "project:unknown-parent"
	got := EffectiveCosts([]CostObservation{one, two, estimate})
	if len(got) != 2 || got[0].ID != "first" || !got[0].Partial || got[1].Partial {
		t.Fatalf("partial flag must affect only first matching accepted report: %+v", got)
	}
	adjacent := one
	adjacent.ID = "adjacent"
	adjacent.StartAt = one.EndAt
	adjacent.EndAt = one.EndAt.Add(time.Hour)
	if got := EffectiveCosts([]CostObservation{one, adjacent}); len(got) != 2 {
		t.Fatalf("half-open adjacent windows overlap: %+v", got)
	}
}

func costLedger(size int, uniqueScopes bool) []CostObservation {
	at := time.Date(2026, 10, 2, 0, 0, 0, 0, time.UTC)
	costs := make([]CostObservation, size)
	for i := range costs {
		scope, start := "request:one", at.Add(time.Duration(i)*time.Second)
		if uniqueScopes {
			scope = fmt.Sprintf("request:%d", i)
			start = at
		}
		costs[i] = CostObservation{ID: fmt.Sprint(i), Provider: "provider", AccountID: "account", Currency: "USD", Kind: "reported", Scope: scope, StartAt: start, EndAt: start.Add(time.Second), ObservedAt: at, Amount: "0.001"}
	}
	return costs
}

func BenchmarkEffectiveCostsLargeLedger(b *testing.B) {
	for _, size := range []int{1000, 10000, 100000} {
		for _, unique := range []bool{true, false} {
			name := "adjacent_windows"
			if unique {
				name = "disjoint_requests"
			}
			b.Run(fmt.Sprintf("%s/%d", name, size), func(b *testing.B) {
				costs := costLedger(size, unique)
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if got := EffectiveCosts(costs); len(got) != size {
						b.Fatalf("lost disjoint costs: %d", len(got))
					}
				}
			})
		}
	}
}

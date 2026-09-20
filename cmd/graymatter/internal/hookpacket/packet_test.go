package hookpacket

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestSelectRanksWholeFactsWithoutMutatingSnapshot(t *testing.T) {
	facts := []string{"other subject", "alpha alpha", "alpha"}
	before := append([]string(nil), facts...)
	got, err := Select(context.Background(), "alpha", facts, 2, 832)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Selected, []int{2, 1}) {
		t.Fatalf("selected %v, want shorter relevant fact first", got.Selected)
	}
	if got.Candidates != 3 || got.Bytes != len("alpha\n\nalpha alpha") || got.OmissionReason != "" {
		t.Fatalf("incorrect receipt: %+v", got)
	}
	if !reflect.DeepEqual(facts, before) {
		t.Fatal("selection mutated snapshot")
	}
	got.Facts[0] = "changed returned slice"
	got.Selected[0] = 99
	if !reflect.DeepEqual(facts, before) {
		t.Fatal("returned slice aliases snapshot")
	}
}

func TestSelectStableTies(t *testing.T) {
	facts := []string{"alpha red", "alpha tan", "alpha sky"}
	for i := 0; i < 25; i++ {
		got, err := Select(context.Background(), "alpha", facts, 2, 832)
		if err != nil || !reflect.DeepEqual(got.Selected, []int{0, 1}) {
			t.Fatalf("run %d: selected %v, error %v", i, got.Selected, err)
		}
	}
}

func TestUnicodeTermsRetainNegationAndIdentifiers(t *testing.T) {
	freq, order, err := frequencies(context.Background(), "ESTÁ esta\u0301 ＮＯ not Straße STRASSE A_2026")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"está": 2, "no": 1, "not": 1, "strasse": 2, "a": 1, "2026": 1}
	if !reflect.DeepEqual(freq, want) || !reflect.DeepEqual(order, []string{"está", "no", "not", "strasse", "a", "2026"}) {
		t.Fatalf("frequencies %v, order %v", freq, order)
	}
	got, err := Select(context.Background(), "not", []string{"allowed now", "not allowed"}, 1, 832)
	if err != nil || !reflect.DeepEqual(got.Selected, []int{1}) {
		t.Fatalf("negation was lost: result %+v, error %v", got, err)
	}
}

func TestSelectBudgetCountsUTF8AndSeparators(t *testing.T) {
	facts := []string{"ñ", "日本", "ab"}
	for _, tc := range []struct {
		budget int
		want   []int
		bytes  int
	}{
		{10, []int{0, 1}, 10},
		{9, []int{0, 2}, 6},
		{2, []int{0}, 2},
		{1, []int{}, 0},
	} {
		got, err := Select(context.Background(), "", facts, 8, tc.budget)
		if err != nil || !reflect.DeepEqual(got.Selected, tc.want) || got.Bytes != tc.bytes {
			t.Fatalf("budget %d: result %+v, error %v", tc.budget, got, err)
		}
		payload := strings.Join(got.Facts, "\n\n")
		if len(payload) != got.Bytes || !utf8.ValidString(payload) {
			t.Fatalf("invalid payload %q", payload)
		}
		if tc.budget == 1 && got.OmissionReason != "no_fact_fits_budget" {
			t.Fatalf("missing budget omission reason: %+v", got)
		}
	}
}

func TestSelectSkipsOversizedTopFactWithoutCuttingIt(t *testing.T) {
	got, err := Select(context.Background(), "target", []string{"target", "other"}, 3, 5)
	if err != nil || !reflect.DeepEqual(got.Facts, []string{"other"}) {
		t.Fatalf("result %+v, error %v", got, err)
	}
}

func TestSelectEmptySnapshot(t *testing.T) {
	got, err := Select(context.Background(), "query", nil, 3, 832)
	if err != nil || got.Candidates != 0 || got.Bytes != 0 || len(got.Facts) != 0 || got.OmissionReason != "no_candidates" {
		t.Fatalf("result %+v, error %v", got, err)
	}
}

func TestSelectRejectsInvalidInputsWithoutPartialPayload(t *testing.T) {
	tooMany := make([]string, MaxCandidates+1)
	tooMuch := []string{
		strings.Repeat("a", MaxTextBytes), strings.Repeat("b", MaxTextBytes),
		strings.Repeat("c", MaxTextBytes), strings.Repeat("d", MaxTextBytes), "e",
	}
	for _, tc := range []struct {
		name     string
		query    string
		facts    []string
		maxFacts int
		maxBytes int
	}{
		{"too many candidates", "q", tooMany, 3, 832},
		{"empty fact", "q", []string{"valid", ""}, 3, 832},
		{"blank fact", "q", []string{"\u3000"}, 3, 832},
		{"duplicate fact", "q", []string{"same", "same"}, 3, 832},
		{"invalid UTF8 fact", "q", []string{"\xff"}, 3, 832},
		{"invalid UTF8 query", "\xff", []string{"valid"}, 3, 832},
		{"oversized fact", "q", []string{strings.Repeat("x", MaxTextBytes+1)}, 3, 832},
		{"oversized query", strings.Repeat("x", MaxTextBytes+1), []string{"valid"}, 3, 832},
		{"oversized snapshot", "q", tooMuch, 3, 832},
		{"zero facts", "q", []string{"valid"}, 0, 832},
		{"negative facts", "q", []string{"valid"}, -1, 832},
		{"excess facts", "q", []string{"valid"}, 9, 832},
		{"zero bytes", "q", []string{"valid"}, 3, 0},
		{"negative bytes", "q", []string{"valid"}, 3, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Select(context.Background(), tc.query, tc.facts, tc.maxFacts, tc.maxBytes)
			if err == nil || !reflect.DeepEqual(got, Result{}) {
				t.Fatalf("expected error and no payload, got %+v, %v", got, err)
			}
		})
	}
}

func TestSelectContextCancellation(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	for _, tc := range []struct {
		ctx  context.Context
		want error
	}{{cancelled, context.Canceled}, {expired, context.DeadlineExceeded}} {
		got, err := Select(tc.ctx, "query", []string{"fact"}, 3, 832)
		if !errors.Is(err, tc.want) || !reflect.DeepEqual(got, Result{}) {
			t.Fatalf("result %+v, error %v, want %v", got, err, tc.want)
		}
	}
	if _, err := Select(nil, "q", []string{"fact"}, 3, 832); err == nil {
		t.Fatal("nil context accepted")
	}
}

func TestSelectCancellationDuringWork(t *testing.T) {
	// This context cancels after entry validation, so the scorer must observe
	// cancellation too rather than checking only at its public boundary.
	ctx := &cancellingContext{Context: context.Background(), remaining: 5}
	got, err := Select(ctx, "a b", []string{"a", "b"}, 3, 832)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, Result{}) {
		t.Fatalf("result %+v, error %v", got, err)
	}
}

type cancellingContext struct {
	context.Context
	remaining int
}

func (c *cancellingContext) Err() error {
	c.remaining--
	if c.remaining <= 0 {
		return context.Canceled
	}
	return nil
}

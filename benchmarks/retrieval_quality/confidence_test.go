package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/angelnicolasc/graymatter/pkg/embedding"
	"github.com/angelnicolasc/graymatter/pkg/memory"
)

// The fixture and gate text were committed before measuring any weight. The
// normalized checksum makes the freeze portable across Git's CRLF conversion.
const confidenceFixtureSHA = "2ba5626bb91f9883282a5d5a66f3f2cf3278aa7e871ee7000ad2468b6f07ef12"
const confidenceFixtureWindowsSHA = "c05e8a380c47e793d6911e2ffebb1261ed299d304ff9785cff9640cd70c16600"

type confidenceFixture struct {
	Policy   string             `json:"policy"`
	Frozen   bool               `json:"frozen_before_measurement"`
	Weights  []float64          `json:"weights"`
	TopK     int                `json:"top_k"`
	Gates    map[string]string  `json:"gates"`
	Families []confidenceFamily `json:"families"`
}
type confidenceFamily struct {
	Family   string              `json:"family"`
	Split    string              `json:"split"`
	Kind     string              `json:"kind"`
	Variants []confidenceVariant `json:"variants"`
}
type confidenceVariant struct {
	Language string                  `json:"language"`
	Query    string                  `json:"query"`
	Relevant []string                `json:"relevant_keys"`
	Facts    []confidenceFixtureFact `json:"facts"`
}
type confidenceFixtureFact struct {
	Key        string `json:"key"`
	Text       string `json:"text"`
	Confidence string `json:"confidence"`
	AgeHours   int    `json:"age_hours"`
}

type confidenceRankEvidence struct {
	// ID is the fixture key mapped from the canonical committed fact ID. The
	// mapping removes generated ULID noise without substituting text identity.
	ID          string  `json:"id"`
	Base        float64 `json:"base_score"`
	Final       float64 `json:"final_score"`
	Factor      float64 `json:"factor"`
	Label       string  `json:"raw_confidence"`
	Effective   string  `json:"effective_confidence"`
	KeywordRank int     `json:"keyword_rank"`
	RecencyRank int     `json:"recency_rank"`
}
type confidenceOutcome struct {
	Ranks        []confidenceRankEvidence `json:"ranks"`
	Top1Relevant bool                     `json:"top1_relevant"`
	RecallAtK    float64                  `json:"recall_at_k"`
}
type confidenceCaseEvidence struct {
	Family                         string            `json:"family"`
	Language                       string            `json:"language"`
	Kind                           string            `json:"kind"`
	Query                          string            `json:"query"`
	Baseline                       confidenceOutcome `json:"baseline"`
	Candidate                      confidenceOutcome `json:"candidate"`
	VerifiedFilter                 confidenceOutcome `json:"verified_filter"`
	VerifiedEligibleRelevant       int               `json:"verified_eligible_relevant"`
	VerifiedNoRelevantEvidence     bool              `json:"verified_no_relevant_evidence"`
	TargetFormulaRequiresPromotion bool              `json:"target_formula_requires_promotion"`
	Failures                       []string          `json:"gate_failures"`
}
type confidenceWeightEvidence struct {
	Split                         string                   `json:"split"`
	Weight                        float64                  `json:"weight"`
	Passed                        bool                     `json:"passed"`
	TargetImprovements            int                      `json:"target_improvements"`
	ProtectedLosses               int                      `json:"protected_losses"`
	ProtectedBaselineRelevant     int                      `json:"protected_baseline_relevant_top1"`
	AnswerableCases               int                      `json:"answerable_cases"`
	MeanAnswerableRecallBaseline  float64                  `json:"mean_answerable_recall_at_k_baseline"`
	MeanAnswerableRecallCandidate float64                  `json:"mean_answerable_recall_at_k_candidate"`
	MeanTop1Baseline              float64                  `json:"mean_top1_baseline"`
	MeanTop1Candidate             float64                  `json:"mean_top1_candidate"`
	MeanRecallAtKBaseline         float64                  `json:"mean_recall_at_k_baseline"`
	MeanRecallAtKCandidate        float64                  `json:"mean_recall_at_k_candidate"`
	NoVerifiedRelevantEvidence    int                      `json:"no_verified_relevant_evidence"`
	Cases                         []confidenceCaseEvidence `json:"cases"`
}
type confidenceEvaluation struct {
	Policy                string                     `json:"policy"`
	FixtureSHA256         string                     `json:"fixture_sha256_lf"`
	OriginalWindowsSHA256 string                     `json:"original_windows_sha256"`
	TimestampAnchor       string                     `json:"timestamp_anchor"`
	TopK                  int                        `json:"top_k"`
	GateText              map[string]string          `json:"gates"`
	Calibration           []confidenceWeightEvidence `json:"calibration"`
	SelectedWeight        *float64                   `json:"selected_weight,omitempty"`
	Validation            *confidenceWeightEvidence  `json:"validation,omitempty"`
	FixtureGatesPassed    bool                       `json:"fixture_gates_passed"`
	DefaultApproved       bool                       `json:"default_promotion_approved"`
	DefaultWeight         float64                    `json:"current_default_weight"`
	Decision              string                     `json:"decision"`
	Limits                string                     `json:"limits"`
}

type confidenceCaseStore struct {
	family   confidenceFamily
	variant  confidenceVariant
	store    *memory.Store
	idToKey  map[string]string
	baseline []memory.RecallReceipt
}

func loadConfidenceFixtures(t *testing.T) confidenceFixture {
	t.Helper()
	data, err := os.ReadFile("confidence-fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	normalized := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if sum := fmt.Sprintf("%x", sha256.Sum256(normalized)); sum != confidenceFixtureSHA {
		t.Fatalf("frozen confidence fixture changed: %s", sum)
	}
	var fixture confidenceFixture
	if err := json.Unmarshal(normalized, &fixture); err != nil {
		t.Fatal(err)
	}
	if !fixture.Frozen || fixture.Policy != memory.ConfidencePolicy || fixture.TopK != 3 || !reflect.DeepEqual(fixture.Weights, []float64{.1, .2, .3, .4, .5}) {
		t.Fatal("invalid frozen evaluation contract")
	}
	seen := map[string]bool{}
	counts := map[string]int{}
	for _, family := range fixture.Families {
		if seen[family.Family] {
			t.Fatalf("family crosses splits: %s", family.Family)
		}
		seen[family.Family] = true
		if family.Split != "calibration" && family.Split != "validation" {
			t.Fatalf("unknown split %q", family.Split)
		}
		counts[family.Split]++
		languages := map[string]bool{}
		for _, variant := range family.Variants {
			languages[variant.Language] = true
		}
		if len(family.Variants) != 2 || !languages["en"] || !languages["es"] {
			t.Fatalf("family %s lacks within-split bilingual variants", family.Family)
		}
	}
	if counts["calibration"] != 12 || counts["validation"] != 12 {
		t.Fatalf("split families=%v", counts)
	}
	return fixture
}

func seedConfidenceCase(t *testing.T, family confidenceFamily, variant confidenceVariant, topK int) *confidenceCaseStore {
	t.Helper()
	// Explicit keyword mode prevents provider discovery or paid/network calls.
	s, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir(), Embedder: embedding.AutoDetect(embedding.Config{Mode: embedding.ModeKeyword}), CandidateRetrieval: true, DecayHalfLife: 720 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	c := &confidenceCaseStore{family: family, variant: variant, store: s, idToKey: map[string]string{}}
	anchor := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	seen := map[string]bool{}
	for _, source := range variant.Facts {
		if source.Key == "" || seen[source.Key] {
			t.Fatalf("invalid fact key %q", source.Key)
		}
		seen[source.Key] = true
		opts := memory.WriteOptions{}
		if source.Confidence != "" {
			label := source.Confidence
			opts.Confidence = &label
		}
		f, err := s.PutWithOptionsReturningFact(context.Background(), "evaluation", source.Text, opts)
		if err != nil {
			t.Fatal(err)
		}
		f.CreatedAt = anchor.Add(-time.Duration(source.AgeHours) * time.Hour)
		f.AccessedAt = f.CreatedAt
		if err := s.UpdateFact("evaluation", f); err != nil {
			t.Fatal(err)
		}
		c.idToKey[f.ID] = source.Key
	}
	for _, key := range variant.Relevant {
		if !seen[key] {
			t.Fatalf("unknown relevance key %s", key)
		}
	}
	c.baseline, err = s.RecallExplain(context.Background(), "evaluation", variant.Query, topK)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func relevantConfidenceKey(keys []string, key string) bool {
	for _, candidate := range keys {
		if key == candidate {
			return true
		}
	}
	return false
}

func confidenceOutcomeFromReceipts(t *testing.T, c *confidenceCaseStore, receipts []memory.RecallReceipt, weight float64) confidenceOutcome {
	t.Helper()
	out := confidenceOutcome{Ranks: make([]confidenceRankEvidence, 0, len(receipts))}
	hits := 0
	for i, r := range receipts {
		key, ok := c.idToKey[r.Provenance.FactID]
		if !ok {
			t.Fatalf("receipt has foreign ID %s", r.Provenance.FactID)
		}
		if r.Ranks.VectorRank != 0 || r.Ranks.K != 60 {
			t.Fatalf("unexpected deterministic provider/ranking: %+v", r.Ranks)
		}
		// Compute the baseline independently from the documented signal weights.
		base := 0.0
		if r.Ranks.KeywordRank != 0 {
			base += 1 / (60 + float64(r.Ranks.KeywordRank))
		}
		if r.Ranks.RecencyRank != 0 {
			base += .5 / (60 + float64(r.Ranks.RecencyRank))
		}
		factor := 1 + weight*float64(memory.ConfidenceLevel(r.Provenance.Confidence)-1)
		final := base * factor
		if r.Ranks.FusedScore != base {
			t.Fatalf("base arithmetic mismatch: %.17g != %.17g", r.Ranks.FusedScore, base)
		}
		if r.Ranking != nil && (r.Ranking.BaseScore != base || r.Ranking.Factor != factor || r.Ranking.FinalScore != final || r.Ranking.ConfidenceWeight != weight) {
			t.Fatalf("confidence receipt arithmetic mismatch: %+v", r.Ranking)
		}
		if weight != 0 && r.Ranking == nil {
			t.Fatal("positive preference omitted score receipt")
		}
		out.Ranks = append(out.Ranks, confidenceRankEvidence{ID: key, Base: base, Final: final, Factor: factor, Label: r.Provenance.Confidence, Effective: memory.EffectiveConfidence(r.Provenance.Confidence), KeywordRank: r.Ranks.KeywordRank, RecencyRank: r.Ranks.RecencyRank})
		if relevantConfidenceKey(c.variant.Relevant, key) {
			hits++
			if i == 0 {
				out.Top1Relevant = true
			}
		}
	}
	if len(c.variant.Relevant) > 0 {
		out.RecallAtK = float64(hits) / float64(len(c.variant.Relevant))
	}
	return out
}

func targetFormulaRequiresPromotion(c *confidenceCaseStore, weight float64) bool {
	bestRelevant, bestOther := -1.0, -1.0
	for _, r := range c.baseline {
		final := r.Ranks.FusedScore * (1 + weight*float64(memory.ConfidenceLevel(r.Provenance.Confidence)-1))
		if relevantConfidenceKey(c.variant.Relevant, c.idToKey[r.Provenance.FactID]) {
			if final > bestRelevant {
				bestRelevant = final
			}
		} else if final > bestOther {
			bestOther = final
		}
	}
	return bestRelevant > bestOther && bestRelevant >= 0
}

func evaluateConfidenceWeight(t *testing.T, cases []*confidenceCaseStore, split string, weight float64, topK int) confidenceWeightEvidence {
	t.Helper()
	report := confidenceWeightEvidence{Split: split, Weight: weight, Passed: true, Cases: make([]confidenceCaseEvidence, 0, len(cases))}
	for _, c := range cases {
		result, err := c.store.RecallExplainWithOptions(context.Background(), "evaluation", c.variant.Query, topK, memory.RecallOptions{ConfidenceWeight: &weight})
		if err != nil {
			t.Fatal(err)
		}
		baseline := confidenceOutcomeFromReceipts(t, c, c.baseline, 0)
		candidate := confidenceOutcomeFromReceipts(t, c, result.Receipts, weight)
		// Repeat the same policy only, establishing deterministic IDs/order/score.
		repeat, err := c.store.RecallExplainWithOptions(context.Background(), "evaluation", c.variant.Query, topK, memory.RecallOptions{ConfidenceWeight: &weight})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(candidate, confidenceOutcomeFromReceipts(t, c, repeat.Receipts, weight)) {
			t.Fatalf("nondeterministic confidence ranking: %s/%s", c.family.Family, c.variant.Language)
		}
		verified := "verified"
		filtered, err := c.store.RecallExplainWithOptions(context.Background(), "evaluation", c.variant.Query, topK, memory.RecallOptions{ConfidenceWeight: &weight, MinConfidence: &verified})
		if err != nil {
			t.Fatal(err)
		}
		filterOutcome := confidenceOutcomeFromReceipts(t, c, filtered.Receipts, weight)
		eligibleRelevant, eligibleFacts := 0, 0
		for _, f := range c.variant.Facts {
			if memory.ConfidenceLevel(f.Confidence) >= 2 {
				eligibleFacts++
				if relevantConfidenceKey(c.variant.Relevant, f.Key) {
					eligibleRelevant++
				}
			}
		}
		if eligibleFacts == 0 && len(filtered.Receipts) != 0 {
			t.Fatal("verified filter returned an ineligible fact")
		}
		for _, r := range filtered.Receipts {
			if memory.ConfidenceLevel(r.Provenance.Confidence) < 2 {
				t.Fatal("verified filter leaked a lower label")
			}
		}
		evidence := confidenceCaseEvidence{Family: c.family.Family, Language: c.variant.Language, Kind: c.family.Kind, Query: c.variant.Query, Baseline: baseline, Candidate: candidate, VerifiedFilter: filterOutcome, VerifiedEligibleRelevant: eligibleRelevant, VerifiedNoRelevantEvidence: eligibleRelevant == 0, Failures: []string{}}
		if eligibleRelevant == 0 {
			report.NoVerifiedRelevantEvidence++
		}
		if c.family.Kind == "target" {
			evidence.TargetFormulaRequiresPromotion = targetFormulaRequiresPromotion(c, weight)
			if !baseline.Top1Relevant && candidate.Top1Relevant {
				report.TargetImprovements++
			}
			if evidence.TargetFormulaRequiresPromotion && !candidate.Top1Relevant {
				evidence.Failures = append(evidence.Failures, "target formula requires promotion but top-1 is irrelevant")
			}
		}
		if c.family.Kind == "protected" && baseline.Top1Relevant {
			report.ProtectedBaselineRelevant++
		}
		if c.family.Kind == "protected" && baseline.Top1Relevant && !candidate.Top1Relevant {
			report.ProtectedLosses++
			evidence.Failures = append(evidence.Failures, "protected baseline top-1 lost")
		}
		if c.family.Kind == "unverified" && candidate.RecallAtK != 1 {
			evidence.Failures = append(evidence.Failures, "sole relevant unverified missing from recall@k")
		}
		if c.family.Kind == "legacy" {
			if len(c.baseline) != len(result.Receipts) {
				t.Fatal("legacy corpus result length changed")
			}
			for i, r := range result.Receipts {
				b := c.baseline[i]
				if r.Provenance.FactID != b.Provenance.FactID || r.Ranks.FusedScore != b.Ranks.FusedScore || candidate.Ranks[i].Final != b.Ranks.FusedScore {
					t.Fatalf("legacy IDs/order/scores changed: %s/%s", c.family.Family, c.variant.Language)
				}
			}
		}
		if len(evidence.Failures) > 0 {
			report.Passed = false
		}
		if baseline.Top1Relevant {
			report.MeanTop1Baseline++
		}
		if candidate.Top1Relevant {
			report.MeanTop1Candidate++
		}
		report.MeanRecallAtKBaseline += baseline.RecallAtK
		report.MeanRecallAtKCandidate += candidate.RecallAtK
		if len(c.variant.Relevant) > 0 {
			report.AnswerableCases++
			report.MeanAnswerableRecallBaseline += baseline.RecallAtK
			report.MeanAnswerableRecallCandidate += candidate.RecallAtK
		}
		report.Cases = append(report.Cases, evidence)
	}
	if report.TargetImprovements == 0 {
		report.Passed = false
	}
	if len(cases) > 0 {
		n := float64(len(cases))
		report.MeanTop1Baseline /= n
		report.MeanTop1Candidate /= n
		report.MeanRecallAtKBaseline /= n
		report.MeanRecallAtKCandidate /= n
	}
	if report.AnswerableCases > 0 {
		n := float64(report.AnswerableCases)
		report.MeanAnswerableRecallBaseline /= n
		report.MeanAnswerableRecallCandidate /= n
	}
	return report
}

func TestConfidenceFrozenCalibrationAndHoldout(t *testing.T) {
	fixture := loadConfidenceFixtures(t)
	report := confidenceEvaluation{Policy: fixture.Policy, FixtureSHA256: confidenceFixtureSHA, OriginalWindowsSHA256: confidenceFixtureWindowsSHA, TimestampAnchor: "2026-01-01T00:00:00Z", TopK: fixture.TopK, GateText: fixture.Gates, DefaultWeight: memory.DefaultConfidenceWeight,
		Limits: "Small handcrafted bilingual fixtures with declared relevance and deterministic keyword-only retrieval. They are not proof of truth, calibrated probabilities, general ranking improvement, model quality, or reproduction of a private store. The validation protected baseline-relevant top-1 count is zero, so that no-loss gate is vacuous on holdout. At the selected weight, credential-rotation, queue-durability, and database-isolation remain incorrect at top-1 in both languages; the frozen formula does not require their promotion. Negative cases report lack of eligible relevant evidence; RRF does not implement abstention. Timestamp ranks are fixed; generated fact IDs are mapped to fixture keys."}
	var calibration []*confidenceCaseStore
	for _, family := range fixture.Families {
		if family.Split == "calibration" {
			for _, variant := range family.Variants {
				calibration = append(calibration, seedConfidenceCase(t, family, variant, fixture.TopK))
			}
		}
	}
	for _, weight := range fixture.Weights {
		outcome := evaluateConfidenceWeight(t, calibration, "calibration", weight, fixture.TopK)
		report.Calibration = append(report.Calibration, outcome)
		if report.SelectedWeight == nil && outcome.Passed {
			candidate := weight
			report.SelectedWeight = &candidate
		}
	}
	if report.SelectedWeight != nil {
		// Validation stores are not queried until calibration has selected one
		// candidate. Only that candidate is evaluated; no holdout retries.
		var validation []*confidenceCaseStore
		for _, family := range fixture.Families {
			if family.Split == "validation" {
				for _, variant := range family.Variants {
					validation = append(validation, seedConfidenceCase(t, family, variant, fixture.TopK))
				}
			}
		}
		outcome := evaluateConfidenceWeight(t, validation, "validation", *report.SelectedWeight, fixture.TopK)
		report.Validation = &outcome
		report.FixtureGatesPassed = outcome.Passed
	}
	switch {
	case report.SelectedWeight == nil:
		report.Decision = "No weight passed calibration; keep default zero and leave issue 126 open."
	case !report.FixtureGatesPassed:
		report.Decision = "The selected calibration candidate failed validation; keep default zero and reserve new validation families before any redesign."
	default:
		report.Decision = "The candidate passed these fixtures; default promotion still requires a published compatibility notice and a later authorized release."
	}
	if math.IsNaN(report.DefaultWeight) || report.DefaultWeight != 0 {
		t.Fatal("opt-in delivery silently promoted product default")
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if destination := os.Getenv("GRAYMATTER_CONFIDENCE_REPORT"); destination != "" {
		if err := os.WriteFile(destination, append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("confidence evaluation:\n%s", data)
}

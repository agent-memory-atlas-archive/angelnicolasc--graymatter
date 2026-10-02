package server

import (
	"context"
	"encoding/json"
	"github.com/angelnicolasc/graymatter/pkg/memory"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRESTConfidencePreferenceRemainsLegacy(t *testing.T) {
	store, err := memory.Open(memory.StoreConfig{DataDir: t.TempDir(), ConfidenceWeight: 0.5, CandidateRetrieval: true})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	verified, unverified := "verified", "unverified"
	v, err := store.PutWithOptionsReturningFact(context.Background(), "a", "policy limit reviewed", memory.WriteOptions{Confidence: &verified})
	if err != nil {
		t.Fatal(err)
	}
	u, err := store.PutWithOptionsReturningFact(context.Background(), "a", "policy limit provisional", memory.WriteOptions{Confidence: &unverified})
	if err != nil {
		t.Fatal(err)
	}
	v.CreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	u.CreatedAt = v.CreatedAt.Add(time.Hour)
	if err := store.UpdateFact("a", v); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateFact("a", u); err != nil {
		t.Fatal(err)
	}
	preferred, err := store.Recall(context.Background(), "a", "policy limit", 1)
	if err != nil || len(preferred) != 1 || preferred[0] != v.Text {
		t.Fatalf("positive setup didn't prefer verified: %v / %v", preferred, err)
	}
	s := New("127.0.0.1:0", testStore{Store: store}, nil)
	w := httptest.NewRecorder()
	s.handleRecall(w, httptest.NewRequest("GET", "/recall?agent=a&q=policy+limit&k=1", nil))
	var out struct {
		Results []string `json:"results"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || len(out.Results) != 1 || out.Results[0] != u.Text {
		t.Fatalf("REST adopted preference: %s / %v", w.Body.Bytes(), err)
	}
	w = httptest.NewRecorder()
	s.handleForget(w, httptest.NewRequest("DELETE", "/forget", strings.NewReader(`{"agent":"a","query":"policy limit"}`)))
	if !strings.Contains(w.Body.String(), u.ID) {
		t.Fatalf("REST query-targeted forget changed selection: %s", w.Body.String())
	}
}

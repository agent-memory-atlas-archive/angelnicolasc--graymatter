package main

import (
	"errors"
	"testing"

	"github.com/angelnicolasc/graymatter/pkg/memory"
)

type confidenceBatchPolicyStore struct {
	cliStore
	readyCalls, defaultCalls int
	readyError               error
}

func (s *confidenceBatchPolicyStore) Ready() error {
	s.readyCalls++
	return s.readyError
}

func (s *confidenceBatchPolicyStore) DefaultConfidenceWeight() float64 {
	s.defaultCalls++
	if s.defaultCalls == 1 {
		return 0.1
	}
	return 0.4 // A restart must not change the policy of an in-flight batch.
}

func TestConfidenceCLIBatchSnapshotsDefaultAfterReady(t *testing.T) {
	store := &confidenceBatchPolicyStore{}
	opts, active, err := cliBatchRecallOptions(store, memory.RecallOptions{})
	if err != nil || !active || opts.ConfidenceWeight == nil || *opts.ConfidenceWeight != 0.1 || store.readyCalls != 1 || store.defaultCalls != 1 {
		t.Fatalf("batch did not capture one connected default: %+v active=%v err=%v store=%+v", opts, active, err, store)
	}
	if store.DefaultConfidenceWeight() != 0.4 || *opts.ConfidenceWeight != 0.1 {
		t.Fatal("a later connection changed the captured policy")
	}
	zero, verified := 0.0, "verified"
	store = &confidenceBatchPolicyStore{}
	opts, active, err = cliBatchRecallOptions(store, memory.RecallOptions{MinConfidence: &verified, ConfidenceWeight: &zero})
	if err != nil || !active || *opts.ConfidenceWeight != 0 || opts.MinConfidence == nil || *opts.MinConfidence != "verified" {
		t.Fatalf("explicit zero lost filter: %+v / %v", opts, err)
	}
	store = &confidenceBatchPolicyStore{readyError: errors.New("connection unavailable")}
	if _, _, err := cliBatchRecallOptions(store, memory.RecallOptions{}); err == nil || store.defaultCalls != 0 {
		t.Fatalf("captured a disconnected/stale default: %+v / %v", store, err)
	}
}

package main

import (
	"context"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
	"github.com/spf13/cobra"
)

func cliWriteOptions(cmd *cobra.Command, confidence string) (memory.WriteOptions, error) {
	var opts memory.WriteOptions
	if cmd.Flags().Changed("confidence") {
		opts.Confidence = &confidence
	}
	return opts, opts.Validate()
}

func cliRecallOptions(cmd *cobra.Command, minimum string, weight float64) (memory.RecallOptions, error) {
	var opts memory.RecallOptions
	if cmd.Flags().Changed("min-confidence") {
		opts.MinConfidence = &minimum
	}
	if cmd.Flags().Changed("confidence-weight") {
		opts.ConfidenceWeight = &weight
	}
	return opts, opts.Validate()
}

type recallSettings struct {
	options memory.RecallOptions
	shared  bool
	all     bool
	legacy  bool
}

func cliBatchRecallOptions(store cliStore, opts memory.RecallOptions) (memory.RecallOptions, bool, error) {
	weight := memory.DefaultConfidenceWeight
	if provider, ok := store.(rpc.DefaultConfidenceProvider); ok {
		if err := store.Ready(); err != nil {
			return opts, false, err
		}
		weight = provider.DefaultConfidenceWeight()
	}
	if opts.ConfidenceWeight != nil {
		weight = *opts.ConfidenceWeight
	}
	useOptions := opts.Requested() || weight != 0
	if useOptions && opts.ConfidenceWeight == nil {
		opts.ConfidenceWeight = &weight
	}
	return opts, useOptions, opts.Validate()
}

func useCLIConfidenceRecall(store cliStore, opts memory.RecallOptions) bool {
	_, supported := store.(rpc.ConfidenceRecaller)
	// Current wrappers negotiate and resolve the effective default on the
	// actual connection, including after a daemon restart. Legacy backends
	// retain their old calls when no new policy was requested.
	return supported || opts.Requested() || memory.DefaultConfidenceWeight != 0
}

func cliRecall(ctx context.Context, store cliStore, agentID, query string, topK int, settings recallSettings) (memory.RecallResult, error) {
	opts := settings.options
	if !settings.legacy && useCLIConfidenceRecall(store, opts) {
		backend, ok := store.(rpc.ConfidenceRecaller)
		if !ok {
			return memory.RecallResult{}, memory.ErrConfidenceUnsupported
		}
		if settings.all {
			return backend.RecallAllWithOptions(ctx, agentID, query, topK, opts)
		}
		if settings.shared {
			agentID = memory.SharedAgentID
		}
		return backend.RecallWithOptions(ctx, agentID, query, topK, opts)
	}
	var result memory.RecallResult
	var err error
	switch {
	case settings.all:
		result.Facts, err = store.RecallAll(ctx, agentID, query, topK)
	case settings.shared:
		result.Facts, err = store.RecallShared(ctx, query, topK)
	default:
		result.Facts, result.Feedback, err = store.RecallDetailed(ctx, agentID, query, topK)
	}
	return result, err
}

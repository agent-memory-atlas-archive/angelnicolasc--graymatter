package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/hookpacket"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
)

const hookPacketBytes = 832

func validHookPacketPolicy(policy string) bool { return policy == "native" || policy == "lexical" }

// These receipts contain counts and reason codes, never prompts or fact text.
// Candidate positions are not persistent fact identifiers.
type hookNamespaceReceipt struct {
	Effective  string `json:"effective"`
	Candidates int    `json:"candidates"`
	Selected   int    `json:"selected"`
	Bytes      int    `json:"bytes"`
	Reason     string `json:"reason,omitempty"`
}

type hookPacketReceipt struct {
	Policy   string               `json:"policy"`
	Unit     string               `json:"unit"`
	MaxBytes int                  `json:"max_bytes"`
	Project  hookNamespaceReceipt `json:"project"`
	Shared   hookNamespaceReceipt `json:"shared"`
}

func hookRecallLexicalBlock(ctx context.Context, store cliStore, agent, query string) (string, hookPacketReceipt, error) {
	project, pr, pe := hookRecallLexicalNamespace(ctx, func(ctx context.Context, k int) ([]string, error) {
		return hookPacketRecall(ctx, store, agent, query, k, false)
	}, query, hookUserPromptAgentTopK)
	shared, sr, se := hookRecallLexicalNamespace(ctx, func(ctx context.Context, k int) ([]string, error) {
		return hookPacketRecall(ctx, store, agent, query, k, true)
	}, query, hookUserPromptSharedTopK)
	r := hookPacketReceipt{Policy: "lexical", Unit: "utf8_bytes", MaxBytes: hookPacketBytes, Project: pr, Shared: sr}
	return renderMemoryBlock(agent, project, shared), r, errors.Join(pe, se)
}

// The general RPC Recall API retains context only for signature compatibility.
// Use its existing timed transport here so the optional hook cannot wait for
// a full default RPC timeout or reconnect after exhausting its own budget.
func hookPacketRecall(ctx context.Context, store cliStore, agent, query string, k int, shared bool) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if reconnecting, ok := store.(*reconnectingStore); ok {
		store = reconnecting.snapshot()
	}
	if remote, ok := store.(daemonStore); ok {
		// CallService's timer starts after net/rpc has sent the request.
		// Closing this hook-owned connection on cancellation also bounds a
		// blocked send; no detached recall goroutine remains behind.
		stopClose := context.AfterFunc(ctx, func() { _ = remote.Close() })
		defer stopClose()
		timeout := time.Duration(userPromptHookTimeout-1) * time.Second
		if deadline, ok := ctx.Deadline(); ok {
			timeout = time.Until(deadline)
		}
		if timeout <= 0 {
			return nil, context.DeadlineExceeded
		}
		if shared {
			var resp rpc.RecallSharedResponse
			err := remote.CallService(rpc.ServiceName, "RecallShared", &rpc.RecallSharedRequest{Query: query, TopK: k}, &resp, timeout)
			return resp.Facts, err
		}
		var resp rpc.RecallResponse
		err := remote.CallService(rpc.ServiceName, "Recall", &rpc.RecallRequest{AgentID: agent, Query: query, TopK: k}, &resp, timeout)
		return resp.Facts, err
	}
	if shared {
		return store.RecallShared(ctx, query, k)
	}
	return store.Recall(ctx, agent, query, k)
}

func hookRecallLexicalNamespace(ctx context.Context, recall func(context.Context, int) ([]string, error), query string, topK int) ([]string, hookNamespaceReceipt, error) {
	r := hookNamespaceReceipt{Effective: "none"}
	if err := ctx.Err(); err != nil {
		r.Reason = "deadline_or_cancelled"
		return nil, r, err
	}
	facts, err := recall(ctx, 32)
	r.Candidates = len(facts)
	reason := "candidate_recall_failed"
	if err == nil {
		var selected hookpacket.Result
		selected, err = hookpacket.Select(ctx, query, facts, topK, hookPacketBytes)
		if err == nil {
			r.Effective, r.Selected, r.Bytes, r.Reason = "lexical", len(selected.Facts), selected.Bytes, selected.OmissionReason
			return selected.Facts, r, nil
		}
		reason = "invalid_selection_input"
	}
	// Never use the first three entries of Recall(32) as a substitute for
	// native Recall(3). Graph enrichment and ranking can differ with topK.
	if err := ctx.Err(); err != nil {
		r.Reason = "deadline_or_cancelled"
		return nil, r, err
	}
	r.Reason = reason
	fallback, fallbackErr := recall(ctx, topK)
	if fallbackErr != nil {
		r.Reason = "native_fallback_failed"
		return nil, r, errors.New("lexical recall and native fallback failed")
	}
	r.Effective, r.Selected, r.Bytes = "native", len(fallback), len(strings.Join(fallback, "\n\n"))
	return fallback, r, fmt.Errorf("lexical recall used native fallback: %s", reason)
}

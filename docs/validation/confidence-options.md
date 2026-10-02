# Confidence options: validation and delivery boundaries

This is the opt-in delivery for issue #126. The product default remains **0**.
The feature is on this branch; it is absent from the latest published release,
v0.19.1. An Unreleased entry is not a published compatibility notice. Default
promotion and issue closure require the later release stage in
[ADR-016](../decisions/016-confidence-options.md).

## Reproducible evidence

The implementation starts from
`bb71971419070c07d36ad91eb1e3a5a9302d8c6a`. The policy and bilingual fixtures were
committed before measurement in `75bbf3b7bd3876f6a3013efe509ff98883beab01`.
The fixture SHA-256 with LF normalization is
`2ba5626bb91f9883282a5d5a66f3f2cf3278aa7e871ee7000ad2468b6f07ef12`;
the Windows CRLF representation is
`c05e8a380c47e793d6911e2ffebb1261ed299d304ff9785cff9640cd70c16600`.
The evaluator rejects any other checksum.

All acceptance tests use temporary stores and homes, keyword or deterministic
embeddings, empty provider credentials, and disabled external model endpoints.
The protocol harness runs real stdin/stdout pipes and authenticated HTTP, each
with direct storage and a daemon. It waits for each dependent response, reopens
the store to verify persistence, and collects its own processes.

Generate per-case ranking evidence with:

```powershell
$env:GOTOOLCHAIN = 'go1.26.7'
$env:GRAYMATTER_CONFIDENCE_REPORT = Join-Path $env:TEMP 'confidence-quality.json'
go test ./benchmarks/retrieval_quality -run '^TestConfidenceFrozenCalibrationAndHoldout$' -count=1 -v
```

Generate synthetic JSON protocol transcripts from the CLI module with:

```powershell
$env:GRAYMATTER_CONFIDENCE_PROTOCOL_DIR = Join-Path $env:TEMP 'confidence-protocol'
go test -run '^TestConfidenceRealMCPTransportMatrix$' -count=1 -v .
```

The transcript files contain fixture requests and responses, not bearer headers.
The real mixed-version harness builds the pinned historical source with
`-trimpath`, logs source and binary SHA-256 checksums, and exercises old/new
clients, unsupported options, restart negotiation, and index v2 → v3 → v2 → v3.
CI fetches history to run this acceptance test; missing baseline source fails
the test rather than silently skipping it. Binary hashes are specific to the
source, toolchain, operating system and build flags.

## Coverage of acceptance cases

| ID | Evidence and assertions |
|---|---|
| T01 | `confidence_write_test.go`, MCP `confidence_test.go`, RPC `confidence_test.go`, CLI `confidence_e2e_test.go`: omission, exact labels, null/types/invalid ranges and no effects before rejection. |
| T02 | First-commit/reopen tests, historical empty/unknown labels, export and explain provenance. |
| T03 | Concurrent duplicate writes return their exact committed IDs, one embedding per write, validated option snapshots and transactional index failure rollback. |
| T04 | Conservative multi-target revision, explicit override, snapshot rejection, no inherited pin, exact lineage and known committed identity on later retirement failure. |
| T05 | Independent RRF/factor arithmetic, zero-weight legacy, inferred neutrality, uniform categories and deterministic final ordering. |
| T06 | Eligible-corpus IDF/ranks, top-k and relevance cuts; empty/large/default k and duplicate categories. |
| T07 | `confidence_vector_test.go`: native eligibility, progressive expansion with explicit exhaustion, excluded leading neighbors, invalid IDs, cancellation, errors and incomplete backend rejection. |
| T08 | Indexed/scan and plain/explain parity with stemming, signal weights, relevance thresholds, multiple namespaces and confidence-only changes. |
| T09 | Confidence-only index invalidation, v2 rebuild, read-only fallback without writes and actual historical binary upgrade/downgrade. |
| T10 | Base and final receipts, effective unknown labels, text/JSON/schema agreement and empty plain/explain/batch results. |
| T11 | Fresh transactional lifecycle patches with deterministic barriers, no resurrection, conservative summaries over validated consumes, extraction provenance and vector reconciliation. |
| T12 | Filtered KG suppression and visible reason; weak-match/usage-alias isolation and retained unfiltered graph-tail exception. |
| T13 | RPC round trips, strict new-request validation, old/new clients and daemons, capability rejection before mutation and known post-commit error receipts. |
| T14 | Wrapper/reconnect tests: per-connection negotiation, semantic unsupported errors without retry/spawn and no uncertain-write replay. |
| T15 | Real four-path MCP matrix: initialize, tools/list and tools/call; add, reflect/update, search, explain, batch and shared scope. |
| T16 | Real CLI remember/revise/shared and plain/explain/all/batch; validation before dispatch, empty receipts and unsupported-operation failures. |
| T17 | External-package legacy `AdvancedStore` implementation compiles without embedding a current interface; old signatures/endpoints and legacy contracts remain tested. |
| T18 | Concurrent per-query weights, project/shared isolation, tombstones/aliases and access writes only for final canonical results, including merged all-scope results. |
| T19 | Frozen 12-family calibration and 12-family holdout, each in English and Spanish; independent scores and explicit absence-of-evidence reporting. |
| T20 | `BenchmarkConfidenceRecall` and `BenchmarkConfidencePut`: 600/5k/10k/30k mixed facts, durable writes, nonempty results, normal access bookkeeping, weight/filter allocations and matched historical measurements; [same-host report](confidence-performance.md) and [unchanged scale gate](confidence-scale-gate.json), both on `aa702248`. |
| T21 | Zero first-stage default, configured-positive direct/RPC/MCP policy receipts, negotiated effective defaults and explicit zero preserving an explicit filter. |

Tests are in `pkg/memory`, `pkg/memory/rpc`, the root public API package and the
nested CLI module. Root `go test ./...` does not include that nested module.

Review regression coverage additionally checks:

- Reopening an indexed store with indexing disabled, changing confidence or
  replacing facts without changing the count, and enabling the index again.
  Filtering, weighting and receipts must agree with canonical facts; a read-only
  open must fall back without repairing the persisted index.
- Consolidating pinned facts with and without indexing performs zero bbolt
  writes. A concurrent pin or unpin is read under the writer lock, preserving
  current confidence and access metadata even when decay has nothing to write.
- Pin, unpin and forget against the pinned historical daemon retain their legacy
  behavior over CLI and MCP. Negotiation happens before mutation; an unsupported
  response or transport error after dispatch cannot trigger a legacy replay.
  Conservative revision still requires the new write capability.

Local validation must clear `ANTHROPIC_API_KEY`, `OPENAI_API_KEY` and
`VOYAGE_API_KEY` and disable the Ollama endpoint in the test process environment.
An inherited consolidation provider can add summary records to legacy daemon
tests that assert a fixed count of user writes.

## Quality result and limits

The [per-case ranking report](confidence-quality.json) records every evaluated calibration weight and the single holdout candidate. The smallest calibration-passing candidate is **0.1**. That single candidate
was evaluated on holdout. Calibration weights 0.1–0.4 pass; 0.5 fails four
protected cases. At 0.1, calibration has eight target top-1 improvements and
holdout has two, with no protected losses under the frozen criteria.

These gates have material limits. The holdout's protected cases have **zero
baseline relevant top-1 answers**, so its no-loss top-1 gate is vacuous. At the
selected weight, credential-rotation, queue-durability and database-isolation
remain wrong at top-1 in both languages; the formula does not require their
promotion. Answerable recall@3 is 1 in these small corpora, which cannot establish
large-corpus recall quality. Verified-only filtering sometimes leaves verified
distractors with no relevant eligible evidence. The scorer does not implement
abstention or contradiction resolution.

Passing these handcrafted keyword-only fixtures establishes the recorded
fixture behavior. It is not evidence of truth, calibrated probabilities,
general ranking improvement or reproduction of a private incident. The report
records `fixture_gates_passed=true` and `default_promotion_approved=false`.
The zero default remains in place pending a real compatibility notice and a
later separately authorized release.

RPC uses the server-configured default advertised on the current connection
unless `DialOptions.DefaultConfidenceWeight` explicitly overrides it. A batch
captures that default once; individual queries cannot switch policy after a
restart. T21 verifies this opt-in contract and pre-confidence capability absence.
The later promotion must additionally verify a newer positive-default client
against an earlier confidence-v1 daemon advertising zero, and publish the
intended upgrade/default-ownership behavior. This future release scenario is
pending, not covered by a claim of completed promotion.

## Integration gates and release steps

Run the commands in both workspace modules, with Go 1.26.7 locally and the
existing CI matrix of Ubuntu/macOS/Windows × Go 1.25/1.26.7:

```text
go build ./...
go build ./cmd/graymatter
go vet ./...
go test ./...
go test -race -count=1 -timeout=600s ./pkg/memory/...
go test -race -count=1 -timeout=300s .
go test -race -count=1 -timeout=1200s ./benchmarks/token_count/... ./benchmarks/retrieval_quality/... ./benchmarks/revision_currency/...
go test -race -short -count=1 ./benchmarks/hook_latency/

# From cmd/graymatter:
go vet ./...
go test ./...
go test -race -count=1 -timeout=300s ./internal/mcp/... ./internal/daemon/...
go test -race -count=1 -timeout=300s .
```

CI retains the existing per-platform coverage gates (core ≥70%, CLI ≥65%),
union gates (core ≥82%, CLI ≥72%), workflow/MCP contracts, version consistency
and vulnerability check. Clock measurements are separate from deterministic
acceptance, use alternating baseline/candidate samples on the same host, and
retain nonempty result checks. Do not interpret shared-runner timing noise as a
correctness failure or lower existing budgets to make a result pass.

The next release must first announce the future bounded preference and show
`confidence_weight: 0` / `--confidence-weight 0` as the opt-out. A later release
may promote a revalidated positive weight. Neither merge, publication, positive
default nor issue closure is implied by this opt-in delivery.

Rollback of preference sets weight to zero while keeping any explicit filter
and stored confidence metadata. Upgrade/restart an old daemon before requesting
new confidence semantics. Do not erase labels or silently change a running
daemon's policy.

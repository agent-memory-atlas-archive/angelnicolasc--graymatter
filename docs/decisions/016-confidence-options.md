# ADR-016: Explicit confidence with a bounded retrieval preference

Status: Accepted for opt-in implementation; default promotion pending.

## Context

Confidence metadata alone did not implement confidence writes and retrieval
through MCP. A post-write text scan can label the wrong duplicate, and adding
fields to existing RPC requests lets older daemons silently ignore them.

## Decision

Write text, ID, declared confidence, derived index and vector queue from the
same fact transaction. Keep existing Go interfaces and add narrow optional
capabilities. New inputs accept exactly verified, inferred and unverified.
Omission on add preserves empty legacy metadata. Empty historical metadata is
effectively inferred; unknown historical metadata remains stored and is
effectively unverified. These are writer declarations, not truth probabilities.

On revision and generated summaries, omitted confidence is the minimum of
inferred and the effective categories of the validated live inputs. Revalidate
IDs and confidence snapshots before committing the replacement. Explicit
revision labels override this rule. Generated extraction is unverified; a
fallback retaining original input keeps ordinary legacy storage. Do not inherit
pin. Narrow lifecycle patches preserve current confidence and tombstones.

Filter the live, non-alias corpus before IDF, signal ranks, fusion, relevance
cuts, deduplication and top-k. Preserve lineage from all relevant tombstones.
Filtered vector queries must verify eligible neighbors or report an unsupported
capability/incomplete search. Embed each query once. The compact derived index
gets a new version and rebuilds from canonical facts; old read-only indexes
fall back to scan without writing.

Keep base RRF B and its stable fused_score receipt. Apply final score
B * (1 + w*c), with c=1 for verified, 0 for inferred, -1 for unverified,
and finite w in [0, 0.5]. Order by final score descending, creation ascending,
then ID ascending. Apply MinRelevance to final scores. Zero weight keeps an
explicit filter. Confidence adds no score when B is zero and never promises
absolute priority over relevance. Receipts expose both scores, factor, effective
confidence, weight and policy confidence-v1.

Any explicit confidence filter suppresses graph neighbor labels, which have no
verifiable confidence receipt. Metadata and text explain this suppression.
Unfiltered graph hints keep their legacy limit and plain/explain exception.
Feedback and alias learning cannot reintroduce excluded evidence.

RPC uses new endpoints and versioned Ping capabilities, negotiated per
connection. Legacy endpoints retain zero weight. A new semantic request to an
old daemon fails before effects; uncertain writes are never blindly replayed.
Explicit zero also requires support when the caller requests policy metadata.

An omitted RPC weight uses the configured default advertised by the actual
server connection; an explicit `DialOptions.DefaultConfidenceWeight` overrides
that client default. Adapters bind the effective weight before sending it, and
a batch binds it once before fan-out. Compatibility endpoints stay at zero.

## Rollout and evidence

The first delivery defaults to zero. No published notice of future preference
exists in latest v0.19.1; Unreleased is not a delivered notice. Promotion needs
an actual minor-release notice and a later minor release with a passing default
candidate. Preserve explicit zero opt-out and mixed-version negotiation. The
REST application retains legacy behavior. This delivery references issue 126
without closing it.

Future promotion must also validate a newer positive-default client against
an earlier confidence-v1 opt-in daemon advertising zero. The current tests
cover configured-positive negotiation and pre-confidence capability absence;
they do not approve that future distribution skew. Upgrade/restart guidance
and the intended server/client default ownership must remain explicit in the
promotion release.

Freeze benchmarks/retrieval_quality/confidence-fixtures.json before measurement:
12 calibration families and 12 disjoint validation families, each with English
and Spanish variants. Candidate weights are 0.1 through 0.5. Select the smallest
calibration-passing candidate and evaluate it once on holdout. Report every
family and language, top-1, recall@k, no eligible relevant evidence, and sample
limitations. Do not change labels/splits/gates to approve a failing default.

## Alternatives and consequences

A fourth category-ranked RRF signal depends on category population and changes
the neutral legacy corpus. Sorting by category grants absolute priority to
irrelevant verified facts. Both are rejected. The bounded multiplicative formula
keeps inferred neutral but can still promote irrelevant recency matches; this
is why protected distractor gates and staged rollout are required.

## Reversal condition

Block promotion if any protected case loses its only relevant answer, any route
ignores options, or receipts cannot reconstruct selection. Set weight to zero
to reverse preference while retaining filters and labels. A distributed default
reversal requires a documented release; never erase confidence or silently
change a running daemon's policy.

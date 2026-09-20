# Lexical selection for prompt hooks

The experimental `lexical` policy gives `UserPromptSubmit` a wider pool to
select from while keeping the injected context bounded. It runs locally in
Go, with no additional model, process, or download. The default is `native`.

## Enable or return to native

```sh
graymatter hooks install --packet-policy lexical
graymatter hooks doctor

# Return to the existing recall policy.
graymatter hooks install --packet-policy native
```

Use the same `--scope project|global` as your installed hooks. Reinstalling
without `--packet-policy` preserves an explicit choice. A fresh installation
without the flag follows the product default. Doctor reports the policy and
whether it is explicit or inherited. Uninstall works as before; changing the
policy does not migrate or restore memory data.

The flag also works on `graymatter hooks run user-prompt`. Other events and
the `remember:` / `remember shared:` paths keep their existing behavior.
Hook input JSON cannot override the selection policy or its limits.

## Selection contract

Each project and shared namespace is processed independently:

1. Retrieve up to 32 candidates through the existing recall path.
2. Normalize words with Unicode NFKC and case folding, retaining accents and
   negation. Rank by TF-IDF cosine similarity divided by the square root of
   the fact's UTF-8 byte length. Ties retain candidate order.
3. Select at most three whole facts within 832 UTF-8 payload bytes, including
   the two-newline separators. Render the existing memory block.

Headers, markers and bullet formatting are additional bytes. This is a byte
budget, not a token count. No budget transfers between namespaces. Facts
that do not fit are omitted, never cut mid-sentence. If none fit, that
namespace contributes no facts; this does not establish that memory has no
answer.

Explicit MCP searches retain their existing ranking and remain available for
more evidence. The policy does not change embedding provider selection.

## Reliability and diagnostics

Invalid or oversized candidate sets, including graph enrichment beyond 32
facts, trigger a fresh native recall for the affected namespace when time
remains. That fallback follows native limits, including graph enrichment,
rather than the lexical byte cap. The other namespace can still contribute. An expired deadline does
not start another fallback call. Identical rendered blocks remain suppressed
within the same session; changing the selected content allows a new injection.

Lexical requests add a `packet` receipt to `hooks.log`: policy, byte unit,
candidate and selected counts, payload bytes, and fallback/omission reason.
These additional receipts contain neither prompts nor memory text.

## Experimental limits

Lexical similarity does not establish identity, truth, or completeness. A
three-fact packet can omit one side of a conflict or a supporting reference;
use a focused search when the task needs those details. Technical fallback
does not detect these semantic omissions. Evidence motivating this policy
was collected with keyword recall; gains with embedding providers are not
established.

Recall updates access metadata for its entire returned candidate pool, even
for facts the selector omits. Those accesses can affect later decay. This
policy preserves that existing store behavior; it is not a read with no
side effects.

The benchmark in `benchmarks/hook_latency` measures both policies at 500 and
10,000 facts through the real CLI and daemon. Machine-relative gates and
absolute timings are reported separately. Local results do not certify
other hardware or production workloads.

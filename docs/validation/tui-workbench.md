# Workbench validation record

Date: 2026-10-02. Scope: the six-view terminal workbench, safe inspection and
curation, Usage observations, and the offline demo. This record distinguishes
automated fixtures from live provider or terminal certification.

## Acceptance coverage

| Area | Evidence |
|---|---|
| Input and focus | State tests for text input, multiline editors, palette, scoped shortcuts, focus and mouse routing |
| Mutation integrity | Exact fact IDs, stale snapshot conflicts, atomic replacement/retirement, confirmations, dropped responses and no automatic write replay |
| Inspection | Direct and RPC preview parity, unchanged access counts/hooks/aliases/index state, no read-only directory or vector creation |
| Async behavior | Resource generations, namespace changes, stale responses, cancellation, retained last-success observations |
| Layout | Cell bounds at 80×24, 100×30, 120×36 and 160×48; CJK/Unicode, terminal controls, narrow inspectors, dialogs and NO_COLOR |
| Usage | Null versus zero, reset times, account scope, currencies, estimates, overlapping reports, decimal arithmetic, pagination, errors, backoff and concurrent imports |
| Provider contracts | Synthetic HTTP reports, an app-server helper process and statusline JSON; no private account or credential scraping |
| Demo | Provider settings isolated, repeated seeding idempotent, fresh reset accepts its Usage ledger, script retains the offline entrypoint |

The CLI integration fixture now clears inherited provider credentials. Its
concurrent daemon test still requires exactly 80 facts and verifies every
submitted sentence occurs once; it no longer allows environment-dependent
consolidation to add records during that assertion.

## Local test results

Windows amd64, Go 1.26.7, with external provider credentials removed from test
subprocesses:

- The complete root module passed with `-race -short`; the three benchmark
  correctness suites also passed separately without `-short`.
- The complete CLI module passed under the race detector. The entrypoint
  package completed in 250.3 seconds with a 600-second package limit. An earlier
  run reached the old 300-second aggregate limit; no assertion had failed and
  its active test had run for 17 seconds. Internal package limits remain 300
  seconds, and operation deadlines remain enforced.
- The local atomic coverage run measured 84.8% for memory/RPC and 79.5% for the enumerated
  CLI internal packages, including Usage. These are local Windows figures;
  the all-platform union is calculated separately by CI.
- The actual Bubble Tea program lifecycle passed resize, draft submission,
  revision, confirmation and quit under `-race`, in addition to state tests.
- A standalone CLI build with Go 1.25.14 passed, using a temporary external
  module file to reference this checkout. The committed module has no replace
  directive.
- A separate standalone build with an injected version returned the same
  version from `--version` and a real stdio MCP `initialize` request; both
  processes exited successfully. A scratch-store keyword remember/recall
  round trip also passed.
- `go vet ./...`, `govulncheck ./...` and CGO-disabled builds passed in both
  modules. The vulnerability scanner reported no reachable vulnerabilities.
  Inspection, cancellation, daemon adapters, read-only/reconnect behavior and
  the complete Usage package also passed targeted checks with Go 1.25.14.
- Final regressions cover config-only Usage reads without database creation,
  corrupt history without partial success, and cancellation when a configured
  wrapper exits while its descendant retains stdout. The complete Usage race
  suite and CLI Usage tests passed after these fixes.
- The visual revision passed the complete CLI entrypoint race suite in 277.4
  seconds. After the final spacing adjustment, workbench, program-lifecycle and
  Usage race tests passed again, including Page Down, mouse hit targets and
  selection preservation across the 29/30-row spacing breakpoint. CLI vet and
  the CGO-disabled build also passed. Long quota labels, windows and context
  component names remain recoverable in narrow layouts.

The initial feature commit passed all 15 remote checks, including the
Ubuntu/macOS/Windows matrix on Go 1.25 and 1.26.7. CI measured union coverage
of 84.6% for memory/RPC and 79.6% for CLI internals. These results belong to
that commit; later visual revisions require their own CI run.

## Local performance

Windows amd64, AMD Ryzen 5 3400G, Go 1.26.7. These are local measurements, not
cross-platform latency guarantees. Background test load can affect timings.

`BenchmarkListFactsPage` reads the first 100 rows with an exact filtered total.
Canonical facts contain 384-dimensional embeddings. Fixture construction uses
a single transaction and is excluded from the timed section. Three iterations:

| Corpus | Page time | Total allocations per operation |
|---:|---:|---:|
| 100 | 1.46 ms | 89 KB |
| 10,000 | 159.6 ms | 6.54 MB |
| 100,000 | 1.522 s | 68.1 MB |

The result set and transfer are bounded; obtaining an exact count still scans
the namespace. These allocation figures include temporary decoding and do not
claim constant total allocation or process RSS.

The local UI profile uses a 120×36 terminal with 100 visible records, 250 samples
and no provider call. Alternating `j`/`k` changes the selection on every sample:

| Operation | p50 | p95 |
|---|---:|---:|
| Render | 3.999 ms | 5.621 ms |
| Navigate and update inspector | 1.000 ms | 2.058 ms |

These UI measurements exclude corpus loading. `GM_TUI_PROFILE=1` enables the
optional profile test; `BenchmarkWorkbench` covers allocation measurements.
The figures above were refreshed after the visual and adaptive-spacing changes.

Cost selection with 100,000 observations takes approximately 120 ms for
disjoint request scopes and 127 ms for adjacent windows on the same machine.
It uses interval indices instead of pairwise comparison. Randomized parity
tests preserve the prior overlap precedence. This work runs in the asynchronous
Usage loader; the visible observation list is bounded separately.

## Scope of manual verification

The Windows binary was exercised through a ConPTY-backed terminal using ttyd
and xterm.js. Checks include Memory navigation, namespace filtering, Recall
submission with shortcut letters in the query, adding/revising demo facts,
Unicode entry, Usage modes, resizing, and normal exit. A draft survived the
80×24 → 125×37 → 80×24 sequence and was saved successfully. Actual dimension
changes trigger a full redraw; the draft and focus remain intact. Monochrome behavior was
also observed with `NO_COLOR=1`.

The revised visual layout was captured from the running binary with sample
data. The Windows 10 / ConPTY / ttyd / xterm.js path sometimes displays a block
cursor outside editors. A renderer trace emitted the hide-cursor sequence
after entering the alternate screen and no show-cursor sequence before exit;
the exact downstream compatibility cause remains unisolated.

This does not certify Windows Terminal, VS Code's terminal, WSL, SSH/tmux or
native macOS/Linux emulators. The operating-system CI matrix is separate from
emulator compatibility. No real subscription or billing account was used for
provider smoke tests. Eligibility and permission errors remain visible in
the product.

## Reproduction

Use clean provider environment variables for local tests. Root and CLI are
separate Go modules; both must be checked. CI additionally enforces per-platform
and union coverage, standalone builds and injected CLI/MCP version parity.

```sh
go vet ./...
go test -race -count=1 -timeout=600s ./pkg/memory/...
go test -race -count=1 -timeout=300s . ./tools/...
go test -race -count=1 -timeout=1200s ./benchmarks/token_count/... ./benchmarks/retrieval_quality/... ./benchmarks/revision_currency/...
go test -race -short -count=1 ./benchmarks/hook_latency/
go build ./...
go build ./cmd/graymatter
govulncheck ./...

cd cmd/graymatter
go vet ./...
go test -race -count=1 -timeout=600s ./...
govulncheck ./...
```

For benchmarks, use `-run '^$' -bench BenchmarkListFactsPage -benchmem` in
`pkg/memory` and `-bench BenchmarkEffectiveCostsLarge -benchmem` in
`cmd/graymatter/internal/usage`. Absolute timing is report-only; functional and
race assertions are blocking.

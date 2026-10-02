# Confidence options performance evidence

Measured on Windows amd64, AMD Ryzen 5 3400G, Go 1.26.7. The baseline is
`bb71971419070c07d36ad91eb1e3a5a9302d8c6a`; the candidate is the integrated
uncommitted source snapshot based on `75bbf3b7bd3876f6a3013efe509ff98883beab01`.
[The report](confidence-performance.json) identifies both binaries, the source
archive, every candidate memory-package Go file and both benchmark harnesses
by SHA256. Every candidate source hash still matched at measurement completion.

Three rounds alternated baseline/current, current/baseline, baseline/current
on the same host. Each read arm ran 100 queries; each write arm ran 10 durable
writes. The deterministic mixed-label corpus, timestamps and query are identical
between versions. All reads returned eight facts. Filtered warmup results were
checked against canonical verified labels. Setup and warmup are excluded; normal
access bookkeeping and filesystem synchronization are included. The provider is
keyword-only; weight is 0.2 and the filter is `verified`. Legacy `Recall` already
calls `RecallDetailed`, so both arms include existing feedback computation.

Numbers below are medians of three sample means in milliseconds, followed by
their observed minimum and maximum. They are not request-level percentiles.

| Facts | Old Detailed | New Detailed | New weighted | New filtered |
| --- | --- | --- | --- | --- |
| 600 | 1.597 (1.569–1.664) | 1.508 (1.478–1.540) | 1.580 (1.572–1.602) | 1.459 (1.356–1.471) |
| 5,000 | 3.144 (3.018–3.989) | 3.092 (3.027–3.458) | 3.122 (2.993–3.180) | 2.585 (2.574–2.657) |
| 10,000 | 4.578 (4.493–4.688) | 4.440 (4.421–4.449) | 4.350 (4.163–4.841) | 3.346 (3.264–3.552) |
| 30,000 | 8.320 (8.134–9.173) | 8.236 (8.228–8.463) | 10.008 (9.986–10.257) | 6.193 (5.956–6.325) |

| Facts | Old legacy write | New legacy write | Old verified write | New verified write |
| --- | --- | --- | --- | --- |
| 600 | 1.394 (1.348–1.494) | 1.681 (1.416–1.962) | 2.739 (2.653–3.828) | 1.908 (1.393–2.003) |
| 5,000 | 1.634 (1.617–1.819) | 1.706 (1.582–2.313) | 3.327 (3.235–4.812) | 1.664 (1.638–1.692) |
| 10,000 | 1.847 (1.832–2.008) | 1.834 (1.775–1.876) | 3.380 (3.375–3.612) | 1.703 (1.666–1.958) |
| 30,000 | 2.008 (1.956–2.372) | 1.988 (1.881–2.072) | 3.658 (3.587–3.665) | 2.064 (1.908–2.166) |

Active confidence has measurable allocation cost. Median bytes/allocations per
operation for Detailed and active read arms:

| Facts | Old Detailed | New Detailed | New weighted | New filtered |
| --- | --- | --- | --- | --- |
| 600 | 105,904 / 546 | 104,345 / 547 | 133,445 / 577 | 138,658 / 580 |
| 5,000 | 408,968 / 901 | 407,088 / 901 | 642,113 / 1,035 | 683,114 / 1,137 |
| 10,000 | 747,647 / 1,011 | 745,748 / 988 | 1,193,889 / 1,121 | 1,274,486 / 1,314 |
| 30,000 | 2,058,001 / 1,387 | 2,056,281 / 1,385 | 3,815,524 / 1,609 | 3,828,596 / 2,098 |

At 30k, weighted versus current Detailed costs 21.5% more latency, 85.6% more
bytes and 16.2% more allocations. Filtering reduces latency by 24.8% while
increasing bytes by 86.2% and allocations by 51.5%. At 30k, legacy writes allocate
124,481 / 598 before and 124,893 / 600 after; verified writes allocate
209,138 / 1,317 before and 124,292 / 598 after. The historical verified writer
used a second metadata transaction; the new primitive commits metadata and
returns its identity together.

Legacy read median changes remain within -5.4% to +2.9%. The 600-fact legacy
write median increases 20.6%, with overlapping short-sample ranges; the other
sizes range from -1.1% to +4.4%. These observations neither establish statistical
significance nor erase the measured cost. The small experiment does not cover
vector expansion, concurrent writers, all supported operating systems or Go
versions. Existing performance budgets remain unchanged.

The existing `TestP4ScaleGate` remains a separate acceptance check: indexed
Recall p99 at 30k must be at most 40ms, indexed Recall p99 at 600 at most 15ms,
and indexed Put p50 at most 3ms. Benchmark
sample means above cannot establish these percentile gates. Run the unchanged
gate separately on a quiet host after correctness tests:

```powershell
$env:GOTOOLCHAIN = 'go1.26.7'
$env:GRAYMATTER_SCALE_GATE = '1'
go test ./pkg/memory -run '^TestP4ScaleGate$' -count=1 -timeout=1200s -v
```

Reproduction uses the tracked `BenchmarkConfidenceRecall` and
`BenchmarkConfidencePut` harness. To build the old version, use the matching
[historical API harness](confidence-performance-baseline.go.txt), which changes
only the unavailable option calls. Full samples and return counts are in the
[JSON report](confidence-performance.json); raw test output is in
`confidence-performance-raw/`. The setup uses bounded transactions and stamps
the index only after committed bbolt statistics are available.

Raw sample headers have trailing whitespace normalized; all reported metrics remain unchanged.

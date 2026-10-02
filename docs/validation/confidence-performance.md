# Confidence options performance evidence

Measured on Windows amd64, AMD Ryzen 5 3400G, Go 1.26.7. The baseline is
`bb71971419070c07d36ad91eb1e3a5a9302d8c6a`; the candidate is the committed source
`aa7022480c16464ec4e635589a4d69e05366b4af`.
[The report](confidence-performance.json) identifies both binaries, the source
archive, every candidate memory-package Go file and both benchmark harnesses
by SHA256. The embedding dependencies and module files are also hashed. Every
candidate source hash still matched at measurement completion. Build commands,
exit codes and actual executed test-binary hashes are retained. Public paths are
portable; original external captures remain preserved separately.

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
| 600 | 1.644 (1.535–1.841) | 1.595 (1.501–1.667) | 1.679 (1.636–1.818) | 1.509 (1.407–1.535) |
| 5,000 | 2.946 (2.926–2.955) | 3.035 (3.003–3.315) | 3.133 (2.930–3.224) | 2.733 (2.615–2.792) |
| 10,000 | 4.228 (4.210–4.264) | 4.378 (4.347–4.387) | 4.440 (4.352–4.880) | 3.391 (3.291–3.631) |
| 30,000 | 8.208 (8.138–8.292) | 7.726 (7.551–7.968) | 9.923 (9.826–9.926) | 5.709 (5.698–5.841) |

| Facts | Old legacy write | New legacy write | Old verified write | New verified write |
| --- | --- | --- | --- | --- |
| 600 | 1.413 (1.290–1.421) | 1.475 (1.303–1.491) | 2.791 (2.705–3.205) | 1.453 (1.441–1.673) |
| 5,000 | 1.583 (1.496–1.676) | 1.646 (1.594–1.932) | 3.372 (3.246–3.439) | 1.763 (1.651–2.510) |
| 10,000 | 1.789 (1.654–1.815) | 1.814 (1.768–2.331) | 3.317 (3.259–4.518) | 1.770 (1.659–1.808) |
| 30,000 | 2.161 (2.111–2.232) | 2.050 (2.032–2.089) | 3.733 (3.678–3.913) | 1.917 (1.900–2.002) |

Active confidence has measurable allocation cost. Median bytes/allocations per
operation for Detailed and active read arms:

| Facts | Old Detailed | New Detailed | New weighted | New filtered |
| --- | --- | --- | --- | --- |
| 600 | 106,062 / 547 | 104,320 / 547 | 133,528 / 577 | 138,658 / 580 |
| 5,000 | 408,709 / 900 | 407,182 / 924 | 641,847 / 1,034 | 683,075 / 1,137 |
| 10,000 | 747,631 / 1,011 | 745,968 / 988 | 1,193,642 / 1,113 | 1,275,007 / 1,314 |
| 30,000 | 2,058,877 / 1,387 | 2,056,721 / 1,409 | 3,815,319 / 1,607 | 3,828,569 / 2,098 |

At 30k, weighted versus current Detailed changes latency by +28.4%,
bytes by +85.5% and allocations by +14.1%.
Filtering changes latency by -26.1%, bytes by +86.1%
and allocations by +48.9%. At 30k, legacy writes allocate
124,541 / 598 before and 124,901 / 599 after;
verified writes allocate 209,260 / 1,317 before and
124,661 / 599 after. The historical verified writer
used a second metadata transaction; the new primitive commits metadata and
returns its identity together.

Legacy read median changes range from -4.6% to +5.2%,
and legacy write changes range from -5.2% to +4.4%.
These observations neither establish statistical significance nor erase the
measured cost. Three short fixed-count samples are sensitive to local variance;
all ranges and raw observations are retained. The experiment does not cover
vector expansion, concurrent writers, all supported operating systems or Go
versions. Existing performance budgets remain unchanged.

The existing `TestP4ScaleGate` is a separate acceptance check: indexed Recall
p99 at 30k must be at most 40ms, indexed Recall p99 at 600 at most 15ms and
indexed Put p50 at most 3ms. Benchmark sample means cannot establish these
percentile gates. Run the unchanged gate separately on a quiet host after
correctness tests:

```powershell
$env:GOTOOLCHAIN = 'go1.26.7'
$env:GRAYMATTER_SCALE_GATE = '1'
go test ./pkg/memory -run '^TestP4ScaleGate$' -count=1 -timeout=1200s -v
```

The unchanged gate **passed** on `aa7022480c16464ec4e635589a4d69e05366b4af` with exit code 0. The run took 402.9s; [metadata](confidence-scale-gate.json) and [full output](confidence-scale-gate.txt) retain the unchanged budgets and source checksum.

| Indexed facts | Recall p50 | Recall p99 (minimum of three passes) | Put p50 |
| --- | --- | --- | --- |
| 600 | 1.57ms | 2.66ms | 1.633ms |
| 3,000 | 1.653ms | 9.755ms | 2.088ms |
| 10,000 | 2.095ms | 10.045ms | 2.101ms |
| 30,000 | 3.211ms | 21.175ms | 2.133ms |

This gate exercises the default zero-weight API. It does not establish positive-weight or filtered request-level percentiles.


Reproduction uses the tracked `BenchmarkConfidenceRecall` and
`BenchmarkConfidencePut` harness. To build the old version, use the matching
[historical API harness](confidence-performance-baseline.go.txt), which changes
only the unavailable option calls. Full samples and return counts are in the
[JSON report](confidence-performance.json); raw test output is in
`confidence-performance-raw/`. The setup uses bounded transactions and stamps
the index only after committed bbolt statistics are available. Published raw Go
output uses LF newlines and removes trailing line whitespace only; the JSON
preserves both original and published checksums. Benchmark values remain unchanged.

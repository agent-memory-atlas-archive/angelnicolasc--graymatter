# Hook latency measurement

Run from the repository root, without other builds, race tests, or CPU-heavy
workloads running:

```sh
go run ./benchmarks/hook_latency
```

The existing CI entry point remains:

```sh
go test -count=1 -timeout=900s -v ./benchmarks/hook_latency/
```

For deterministic tests of reporting and environment/receipt validation,
without running the timing measurement:

```sh
go test -short ./benchmarks/hook_latency/
```

The benchmark builds the current CLI and invokes the real `hooks run
user-prompt --packet-policy native|lexical` routes. Each policy uses fresh,
separate stores with 500 and 10,000 project facts and an empty shared namespace.
Every prompt must inject context. Lexical samples must also produce a matching
packet receipt confirming lexical selection; native fallback does not count as
a lexical measurement. Distinct session IDs prevent identical-block suppression
from turning a measured recall into an empty-output result.

Each of the four cells reports one daemon-cold prompt, three warm-up prompts,
and 12 warm measurements of each event. Daemon-cold means that no daemon is
running for that store; the OS file cache is not flushed. Prompt and
`pre-compact` order alternates. `session-end` measurements run afterward so their
detached consolidation cannot contaminate the recall measurements.

The existing gates remain unchanged:

- Median hook-internal `user-prompt` minus `pre-compact` time: at most 200 ms.
- Median hook-internal `session-end` minus `pre-compact` time: at most 200 ms.
- In-process native Recall scaling, normalized by the 20-fold store size:
  at most 2.5 times linear.

The same normalized scaling threshold also applies to each policy's complete
warm hook route. Hook-internal and process wall times are reported separately;
only wall time includes starting the hook process. Additional differences of
p99 hook-internal times use the 200 ms threshold and report breaches separately
from the existing median gates. With only 12 observations, this p99 is the
observed maximum, not a stable tail-latency estimate. CI continues to run this
timing measurement as report-only.

The lexical-minus-native median comparison includes candidate retrieval,
selection, and receipt overhead. It is not an isolated selector microbenchmark
or a randomized causal estimate. No absolute latency guarantee across machines
is inferred from this report.

In-process stores explicitly use keyword embeddings, no consolidation model,
and no background reconciliation. CLI children clear provider credentials and
set the Ollama URL to an unsupported protocol, which `net/http` rejects before
opening a connection. Child daemons and consolidation processes inherit that
environment. Keyword stemming and candidate retrieval are pinned on; usage
alias learning is pinned off. This controls the benchmark without changing
product defaults or reading the user's store.

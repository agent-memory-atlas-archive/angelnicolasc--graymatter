# Usage: account limits, API costs, and context

Graymatter keeps three different measurements separate:

- **Limits** are the provider's reported account quota windows, with their reset times.
- **Spend** contains provider cost reports and clearly labeled local estimates. A subscription's equivalent API price is an estimate, not money charged to the subscription.
- **Context** is the current session's observed occupancy when a host supplies it. It is independent of cumulative tokens and subscription quotas.

The TUI offers Auto, Limits, and Spend views. Sources are opt-in. Starting the TUI or running `usage show` does not discover credentials, run Codex, or contact a billing endpoint. Refresh reads only explicitly enabled connections.

## Storage and commands

Account configuration and observations default to `os.UserConfigDir()/graymatter/usage/`: `%AppData%\graymatter\usage` on Windows, the user configuration directory on Linux, and the application support directory on macOS. `config.json` contains connection metadata and environment variable **names**. `usage.db` stores observations.

The harness also writes project observations to `<--dir>/usage/usage.db`. The TUI combines user and selected project observations, deduplicating identities. It refreshes connections only from user state. The existing harness aggregate stays separately labeled because its records may overlap the newer request ledger.

```sh
graymatter usage show
graymatter usage show --json
graymatter usage show --refresh --json
graymatter usage --state-dir .graymatter show --json
graymatter usage config
```

`--state-dir` selects an explicit usage state root. The normal global `--dir` selects the memory store and harness project ledger; it does not redirect account credentials or configuration. `usage show` reads one state root, while the TUI combines both roots.

`show --refresh` returns a nonzero exit status when any configured refresh fails. It still emits a complete cached snapshot, including successful connections and error statuses, when local state remains readable. JSON stays on stdout; command errors go to stderr. Fatal database errors or cancellation do not produce a usable partial database scan.

## Codex subscription limits

Configure a native Codex executable that is already authenticated through Codex's supported login flow:

```sh
graymatter usage connect codex-personal --kind codex-app-server --account personal --command codex --arg app-server
graymatter usage show --refresh
```

The command is an executable plus separately stored arguments, not a shell expression. On Windows, if an installation exposes only a `.cmd` shim, select its native executable or explicitly configure the appropriate launcher. Graymatter does not add a shell or copy tokens from the launcher. Never put credentials in command arguments.

The adapter opens a short-lived stdio connection and sends only `initialize`, `initialized`, and `account/rateLimits/read`. It prefers the multi-bucket response and falls back to the legacy single bucket. Unknown windows remain `null`. Window duration is in minutes; provider reset timestamps are Unix seconds normalized to UTC.

No model turn, login, quota-reset redemption, purchase, or owner notification is requested. Authentication remains the configured Codex client's responsibility. Server requests for externally managed token refresh are rejected; this adapter does not implement a separate login/token broker.

Reference: [official Codex app-server protocol](https://learn.chatgpt.com/docs/app-server#6-rate-limits-chatgpt).

## Claude Code statusline bridge

Claude Code can pass its documented statusline JSON on stdin:

```sh
graymatter usage statusline --account claude-personal
```

Configure that command through Claude Code's `statusLine` command setting, or invoke it from an existing statusline script with the original JSON on stdin. It consumes exactly one JSON object, persists the supported measurements, and writes no visible statusline text. An existing script can keep rendering its own status line after invoking the bridge. Graymatter does not rewrite Claude settings automatically.

Supported fields are `session_id`, `model.id`, `rate_limits.five_hour`, `rate_limits.seven_day`, `rate_limits.spend_limit`, `context_window`, and the local `cost` counter. The imported account label is required and must stay consistent across updates. Workspace and transcript paths are ignored.

Subscription windows may be absent until the first response or unavailable for the account/host version. Each absent window replaces its earlier observation with an explicit unknown value. Gateway spend-limit percentages can exceed 100%. Reset times are supplied by the host; Graymatter does not infer a renewal time.

`cost.total_cost_usd` is recorded as a **partial list-price estimate for that session**, including on a subscription. Repeated statusline updates replace the cumulative session estimate rather than adding it again. Context input counts combine uncached input, cache reads, and cache writes once; output is not added to Claude Code's reported current input occupancy.

Reference: [Claude Code statusline fields](https://code.claude.com/docs/en/statusline#rate-limit-usage), [Claude Code cost semantics](https://code.claude.com/docs/en/costs).

## Organization API billing reports

Create an appropriate organization admin credential in the provider's own interface. Put it in an environment variable using the operating system's normal secret-management mechanism, then configure only that variable's name:

```sh
graymatter usage connect openai-work --kind openai-costs --account openai-org-work --key-env OPENAI_ADMIN_KEY
graymatter usage connect anthropic-work --kind anthropic-costs --account anthropic-org-work --key-env ANTHROPIC_ADMIN_KEY
graymatter usage show --refresh --json
```

Normal model inference keys may lack the necessary admin scope. The configured account label identifies the organization for deduplication; Graymatter does not infer or verify an organization identity by enumerating accounts. Use distinct labels for distinct organizations, and the same label for imports intended to reconcile with that organization.

The current adapters request organization totals for the **current UTC month**, preserving each returned daily bucket:

| Adapter | Endpoint | Interpretation |
| --- | --- | --- |
| OpenAI | `GET /v1/organization/costs` | Provider-reported major currency units; currency is retained. |
| Anthropic | `GET /v1/organizations/cost_report` | Provider-reported decimal cents converted exactly to USD; always partial because Priority Tier costs are excluded. |

The adapters follow all pagination cursors before replacing any observations from a refresh. A failed or incomplete page sequence leaves the earlier report intact. They do not turn usage tokens into a subscription fee or infer an invoice from a monthly flat subscription. Provider reports can lag activity, and billing adjustments or excluded services can affect coverage.

Connections refresh with at most four concurrent providers and a ten-second deadline per provider. HTTP requests have an eight-second timeout, at most three attempts for 429/5xx responses, bounded retry delays, and a maximum of 100 pages. A stalled provider does not prevent other providers from saving successful results. Completion writes and the final cache read can take up to two additional seconds after cancellation; they do not perform network access. HTTP redirects are rejected so credentials remain on the configured official endpoint. Cost refreshes are throttled for one minute; Codex reads for ten seconds. These are refresh intervals, not promises about provider freshness.

To disable a connection, repeat its complete `connect` command with `--disabled`; saved history remains readable. Repeating a name replaces that connection's configuration. `usage config` displays the current configuration.

References: [OpenAI organization usage and costs](https://platform.openai.com/docs/api-reference/usage/costs), [Anthropic Usage and Cost API](https://platform.claude.com/docs/en/manage-claude/usage-cost-api).

## Gemini and other imported sources

There is no adapter that extracts Gemini CLI OAuth credentials or calls its private Code Assist quota backend. Gemini's native `/stats` display does not establish a supported third-party subscription-quota API. Graymatter accepts a normalized snapshot supplied by a supported exporter, your own telemetry pipeline, or an authorized billing report export.

Gemini CLI's documented headless JSON and telemetry can supply session/model usage. Google Cloud billing exports can supply actual cost reports after their reporting delay. These need mapping into the schema below; this release does **not** connect to BigQuery, scrape `/stats`, run Gemini to generate a billable prompt, or parse an arbitrary vendor CSV automatically.

```sh
graymatter usage import --file usage-snapshot.json
graymatter usage import --file usage-snapshot.json --prices explicit-prices.json
```

`--file -` reads stdin. `--prices` applies a supplied price book to imported request events; it is optional and creates estimates only. Use one provider/model price book per import batch. An unknown price remains unpriced; it is not converted into a zero-dollar bill.

For Gemini, `promptTokenCount` already includes `cachedContentTokenCount`. A mapper must subtract the cached subset to obtain `input_uncached`; never add the full prompt total and cached input together. Keep output/thought/tool quantities separate only when the source defines them as disjoint. Import an explicit unknown quota rather than inventing a percentage from the documented maximum requests per day.

References: [Gemini headless output](https://geminicli.com/docs/cli/headless/), [Gemini telemetry](https://geminicli.com/docs/cli/telemetry/), [Gemini usage metadata](https://ai.google.dev/api/generate-content#UsageMetadata), [Cloud Billing export](https://docs.cloud.google.com/billing/docs/how-to/export-data-bigquery-setup).

## Snapshot schema

The import/export envelope is version 1. Timestamps are timezone-aware RFC 3339 strings; money is a decimal string in major currency units. The following data is synthetic:

```json
{
  "version": 1,
  "events": [{
    "provider": "google",
    "source": "my-supported-telemetry-export",
    "account_id": "api-project-example",
    "request_id": "request-example-1",
    "session_id": "session-example",
    "model": "model-example",
    "time": "2026-10-02T12:00:00Z",
    "operation": "generate",
    "quantities": {"input_uncached": 100, "input_cache_read": 20, "output": 30}
  }],
  "costs": [{
    "provider": "google",
    "source": "authorized-billing-export-example",
    "account_id": "api-project-example",
    "amount": "1.25",
    "currency": "USD",
    "kind": "reported",
    "scope": "project:example",
    "start_at": "2026-10-01T00:00:00Z",
    "end_at": "2026-10-02T00:00:00Z",
    "observed_at": "2026-10-02T12:00:00Z",
    "partial": false
  }],
  "contexts": [{
    "provider": "google",
    "source": "my-supported-telemetry-export",
    "account_id": "api-project-example",
    "session_id": "session-example",
    "model": "model-example",
    "used_tokens": 120,
    "limit_tokens": null,
    "used_percent": null,
    "observed_at": "2026-10-02T12:00:00Z",
    "components": [{"name": "history", "tokens": 110, "estimated": true}]
  }]
}
```

Optional `quotas` entries require `provider`, `account_id`, `source`, `window`, and `observed_at`; they accept `label`, `window_minutes`, `used_percent`, and `reset_at`. A null percentage or reset is unknown, never zero. IDs are derived during import; callers do not choose duplicate identities.

Event identity is provider + source + account + request + operation. A replay preserves the first event time and does not add another event. Changed model, attribution, or quantities for that same identity are rejected atomically. Context identity is provider + account + session + source. Newer observations replace older ones; an old import cannot roll state backward.

Cost identity includes provider, account, kind, scope, currency, and coverage interval. Cumulative `session:ID` and `request:ID` observations are replaced by identity without adding successive updates. Scopes must identify actual coverage consistently. Reported costs take precedence over overlapping estimates in the same account and currency. Distinct request, session, or project IDs at the same scope level are treated as disjoint. Overlap across different scope levels is conservatively suppressed and marked partial when the attribution is ambiguous. Different currencies are never converted or combined. Ambiguous overlapping periods are not prorated. The exported `costs` array retains provenance; consumers should use the package's `EffectiveCosts` policy before adding display totals.

An explicit synthetic price book has this form; replace its values and provenance with the actual applicable provider rates:

```json
{
  "provider": "google",
  "model": "model-example",
  "currency": "USD",
  "source": "synthetic-example-not-provider-pricing",
  "effective_at": "2026-10-01T00:00:00Z",
  "rates": {
    "input_uncached": {"amount": "1", "per_units": 1000000},
    "input_cache_read": {"amount": "0.1", "per_units": 1000000},
    "output": {"amount": "2", "per_units": 1000000}
  }
}
```

An event cannot be priced with a price book whose effective date is later than the event. Missing quantity rates make an estimate partial; if no quantity has a price, estimation fails rather than fabricating a zero. Negative provider cost adjustments are valid, but negative usage quantities and negative unit prices are rejected.

## Provenance, freshness, and coverage

Quota and context snapshots become stale after ten minutes. A quota also becomes stale at its supplied reset instant until a new observation arrives; Graymatter does not reset its value to zero automatically. Recent/open cost buckets become stale when not observed for an hour. Failed connections mark their matching cached source as stale without invalidating unrelated accounts or sources. Historical reports preserve their reported interval and observation timestamp.

Harness request quantities come from successful Anthropic responses and use disjoint cache categories. The harness's context total is observed input plus response output. Its system, memory, history, and task components are text-size heuristics, each marked estimated. They are not silently scaled to fit the observed total. `component_delta` reports the difference when an estimated partition disagrees with the observation. Unknown model capacity remains null. The harness's existing dated model-price table produces only partial estimates; unrecognized models keep their token observations without a fabricated price.

The older per-agent/day harness rollup remains **partial**, even for known model prices. It excludes other clients and may omit embeddings, tools, billing adjustments, and other provider charges. It must not be added to the new request ledger because they can cover the same calls.

## Local data handling

Graymatter reads only the environment variable named in an enabled billing connection during an explicit refresh. It does not scan credential files, copy OAuth tokens, record admin key values, or include HTTP response bodies in errors. Configuration files request owner-only mode and directories request mode 0700 on platforms supporting those modes; Windows uses the user directory's ACL inheritance. Keep an explicitly overridden state directory private.

Imports are bounded to 8 MiB, individual persisted records to 1 MiB, and normalized batches to 10,000 observations. Database reads have record/byte bounds. A separate bbolt database uses short transactions and a one-second lock timeout, so concurrent import processes cannot overwrite each other's request records. Configuration writes replace the file atomically. There is no automatic deletion or retention policy; an exceeded history bound returns an explicit error so operators can archive the state rather than silently losing history.

Tests use synthetic HTTP servers, synthetic JSON, and a helper process implementing the allowed app-server handshake. No test reads private quota data or performs a live billing request.

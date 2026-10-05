# 014 — agent_id is the canonical agent parameter on memory_reflect

**Status:** Accepted — **Date:** 2026-08-27 — **Amended:** 2026-10-05

## Context

Issue #77 recorded a naming asymmetry: `memory_reflect` spelled its agent
parameter `agent` while the other four tools use `agent_id`. Models
generalize parameter names across a toolset, so the asymmetry was a known
silent-failure class; the runtime alias added in v0.15.0 absorbed the calls
but was invisible to clients reading `tools/list`. Step 3 of the issue's plan
— flipping the canonical spelling — was deferred pending a release cycle of
real-world signal; that cycle (v0.15.0 → this change) completed with no
report of the flip causing friction.

The original decision expressed the at-least-one identity rule with a root
`anyOf`. Issue [#139](https://github.com/angelnicolasc/graymatter/issues/139)
exposed a client-compatibility gap: GrayMatter v0.20.0 announces all seven
tools, but Claude Code 2.1.278 without remote schema rewriting excludes
`memory_reflect` during discovery. Removing only that combinator restores the
tool in the same client. This reproduces the mechanism, not the reporter's
exact session; clients with schema rewriting can already accept it. The
[Claude Code schema guidance](https://code.claude.com/docs/en/mcp#tool-input-schemas-with-a-root-level-combinator)
documents this deployment-dependent exclusion.

## Decision

`agent_id` is canonical; `agent` is a deprecated alias:

- The input schema is a flat object with `required: ["action"]` and no root
  `anyOf`, `oneOf`, or `allOf`. It retains `action`, `agent_id`, `agent`, `text`,
  `target`, `confidence`, their types and enums, and
  `additionalProperties: false`. Requiring `agent_id` in the schema would
  reject valid alias-only callers, so both identity properties are optional
  in the schema.
- At least one explicit identity remains mandatory at runtime for both
  stdio and HTTP. The handler rejects missing identity before calling the
  backend; it does not derive an identity from the working directory.
- Every identity field present must be a non-empty string containing at
  least one non-whitespace character. Null, incorrect types, empty strings,
  and whitespace-only strings fail even when the other spelling is valid.
  Validation never falls back from a malformed field into another namespace.
- When both fields are valid, `agent_id` wins, including when the values
  differ. Valid values are used unchanged: `" project "` and `"project"` are
  distinct identities, and Unicode and `__shared__` remain valid.
- The tool and field descriptions explain runtime identity requirements,
  deprecation, and precedence at discovery time.

## Consequences

- Valid canonical-only, alias-only, and dual-field callers keep their
  behavior. Malformed dual-field inputs that previously fell back to the
  alias or ignored an invalid alias now return a tool error before any
  backend read or write. This is scoped to `memory_reflect`; it does not
  enable global SDK argument validation.
- An identity-free object can pass the flat input schema but cannot execute.
  Tests cover both schema compatibility on the real seven-tool `tools/list`
  payload and runtime identity validation with a backend spy. Input-schema
  compatibility does not change `checkpoint_resume`'s union output schema.
- The wire contract tables in `docs/api-stability.md` and the parameter
  guide in `docs/AGENTS.md` teach `agent_id` from now on; `agent` examples
  remain valid code but are no longer the taught form.
- Tool names, actions, annotations, output schemas, result shapes, confidence
  policy, and storage semantics remain unchanged.

## Compatibility amendment

The 2026-10-05 amendment replaces the original root combinator and the
historical fallback of `required: ["action", "agent_id"]`. That fallback
would break alias-only callers in clients that validate inputs. The flat
schema with runtime validation preserves both valid spellings and restores
discovery in clients that reject root combinators. Keep the alias indefinitely
unless a separately announced deprecation cycle justifies removal;
deprecation without a removal date is documentation, not a deadline.

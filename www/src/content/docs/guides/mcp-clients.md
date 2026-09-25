---
title: MCP clients
description: Wiring GrayMatter into Claude Code, Cursor, Codex, OpenCode, Antigravity, Windsurf, VS Code, and any other MCP-compatible client.
---

`graymatter init` auto-wires every supported client at once. Existing entries
from other MCP servers are merged, never overwritten.

`graymatter init --global` still initializes the current project. The flag
also installs home-scoped agent instructions; it does not globalize the
project-scoped MCP configs below. Projects using those configs still need
client wiring, while an existing user-scoped Claude MCP registration can be
reused across repositories. Codex's MCP config is already home-scoped.

| Client | Config file | Scope |
|--------|-------------|-------|
| Claude Code | `.mcp.json` | project |
| Cursor | `.cursor/mcp.json` | project |
| Codex (OpenAI) | `~/.codex/config.toml` | home |
| OpenCode | `opencode.jsonc` | project |
| Antigravity (Google) | `mcp_config.json` | opt-in |
| Windsurf | `.windsurf/mcp.json` | project |
| VS Code Copilot Agent | `.vscode/mcp.json` | project |

**Also works out of the box:** Pi (reads `.mcp.json` natively), Zed, Cline,
and any MCP-compatible client — point them at `graymatter mcp serve`.

## Claude Code with a user-scoped server

Register GrayMatter with Claude once at user scope if you have not already:

```sh
claude mcp add --scope user --transport stdio graymatter -- graymatter mcp serve
```

Global memory instructions can be installed with `graymatter init --global`,
which also performs ordinary setup in the current project. Global hooks are
optional: `graymatter hooks install --scope global` keeps their guard against
unprepared directories.

With MCP and instructions already available, prepare only the current
project's store before a session using a build that includes this Unreleased
flag (the v0.19.1 release does not):

```sh
graymatter init --store-only --quiet
```

The command creates `.graymatter/MEMORY.md` only when neither that marker nor
a regular `gray.db` exists. It does not write client configs, instructions or
hooks, open the DB, or check runtime health. If neither leaf is regular,
symlink or other nonregular entries are rejected; use `--dir` to select the
real data directory. MCP itself can create `gray.db`
when it starts, even without this command. For scripts that locate the Git
checkout or worktree root before launching Claude, see the
[Claude Code launcher examples](https://github.com/angelnicolasc/graymatter/tree/main/examples/claude-global).
Launch MCP and hooks against the same root. A custom `--dir` must also be
configured on each MCP and hook invocation; a single fixed path shares its
store between projects.

## Manual wiring

Any MCP client that accepts a stdio server works with:

```jsonc
{
  "mcpServers": {
    "graymatter": {
      "command": "graymatter",
      "args": ["mcp", "serve"]
    }
  }
}
```

## HTTP transport

For shared setups (multiple agents, one store), run a single server over HTTP:

```bash
graymatter mcp serve --http 127.0.0.1:8080
```

The HTTP transport requires a bearer token; it lives in
`<data-dir>/graymatter.http-token`. Network surfaces bind loopback-only.

## One store, many processes

GrayMatter persists to bbolt, a single-writer embedded DB — only one process
holds the write lock at a time. The CLI and TUI fall back to read-only mode
when the lock is held; a second MCP server fails fast instead of blocking.

Most robust setup: one shared `graymatter mcp serve --http 127.0.0.1:8080`
pointed at by every client. Details and failure modes in the
[agent guide](/reference/agents-guide/).

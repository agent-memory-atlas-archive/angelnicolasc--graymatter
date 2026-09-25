# Project routing for MCP and hooks

GrayMatter selects a project context once when an MCP stdio server or a hook
starts. The context determines the default store and the hook's memory
namespace. MCP tool calls still require an explicit `agent_id`; a project
directory does not grant access or choose that ID for the client. The selection
does not discover a Git root or search parent directories.

## Choosing the project and store

| Invocation | Project root, in priority order | Default store |
|------------|---------------------------------|---------------|
| MCP stdio | `CLAUDE_PROJECT_DIR`, then process working directory | `<project root>/.graymatter` |
| Claude hook | `CLAUDE_PROJECT_DIR`, then event `cwd`, then process working directory | `<project root>/.graymatter` |
| MCP HTTP | No project root inferred from clients | `<service working directory>/.graymatter` |

`CLAUDE_PROJECT_DIR`, when present, must name an existing absolute directory.
An empty, relative, missing, or inaccessible value is an error rather than a
signal to fall back. A nonempty hook payload `cwd` must likewise be an
existing absolute directory. When both are present, a payload directory
inside the Claude project is accepted, including an alias to the same
directory. A payload outside it, such as a sibling worktree, is rejected
before the hook opens a store or writes a log. The rejected hook keeps its
fail-soft empty stdout and exit status 0, with a brief error on stderr.

Set a stable `CLAUDE_PROJECT_DIR` for sessions that invoke hooks from
subdirectories. Without it, each hook event may select a different project
when the payload changes. Start a new session when switching worktrees so the
host supplies a coherent project root and payload. A user-scoped MCP
registration does not itself choose that root; the host must start its stdio
process with the intended project context.

With no `--dir`, stdio MCP and hooks use `<project>/.graymatter`. An explicit
`--dir` selects that store while leaving the project identity unchanged.
Relative `--dir` values, including `--dir .graymatter`, resolve against the
process working directory captured at startup, **not** against the selected
project root. For example, with process directory `/work/repo/subdir` and
`CLAUDE_PROJECT_DIR=/work/repo`, omitted `--dir` selects
`/work/repo/.graymatter`; explicit `--dir .graymatter` selects
`/work/repo/subdir/.graymatter`. Use an absolute `--dir` to select a custom
store without depending on the launcher directory. An absolute path also
works when the process working directory is unavailable, provided a valid
project root is available for stdio or hooks. HTTP MCP instead uses its
service working directory for a default or relative `--dir`. It ignores
`CLAUDE_PROJECT_DIR`, because remote clients do not share that process's
project context.

`init --store-only`, normal `init`, and other CLI commands retain their own
working-directory-based `--dir` behavior. When normal `init` or
`hooks install` is called with an explicit `--dir`, newly written managed MCP
and hook commands persist the selected absolute store path. Omitted `--dir`
keeps new commands dynamic. An existing custom client entry is preserved by
normal init; check its command and arguments yourself to verify where it
points.

`init --global` additionally installs home-scoped instructions; it does not
turn project-scoped MCP entries into user-scoped ones. To use one
user-scoped Claude MCP registration across projects, launch each session from
the intended checkout or worktree, prepare that store with
`graymatter init --store-only`, and use the same root for hooks and MCP. The
[Claude Code launcher examples](../examples/claude-global/README.md) show
one way to prepare the selected checkout before launch.

## Existing stores and namespace changes

If the project-selected store and the old process-directory store are
different physical directories, and the old one has `gray.db` or
`MEMORY.md`, GrayMatter requires an explicit `--dir`. It also stops if the
old candidate cannot be inspected safely. It does not move, merge, or read
facts to make that decision. Review both paths, then run from the intended
project root or select the old store deliberately:

```sh
graymatter --dir /absolute/path/to/old/.graymatter mcp serve
graymatter --dir /absolute/path/to/old/.graymatter hooks run session-start
```

Hooks derive their `agent_id` from the selected project root's directory
name, even with an explicit store. A hook that previously ran from a nested
directory may therefore use a different namespace after this routing change:
with root `/work/repo` and process directory `/work/repo/subdir`, the hook
uses `repo` rather than `subdir`. Existing facts in `subdir` remain in their
original store and namespace. Inspect the old namespace with an explicit
`agent_id` before any manual export or migration. The `__shared__` namespace
stays in the selected store; it is not federated across projects.

HTTP MCP keeps bearer authentication enabled by default. `--no-auth` remains
an explicit loopback-only exception. Routing does not change authentication
or the requirement for each MCP call to supply its `agent_id`.

## Compatibility and recovery

1. Check the launcher's process working directory and, for Claude Code,
   `CLAUDE_PROJECT_DIR` and hook payload `cwd`. They must identify one
   project; a sibling worktree needs its own session.
2. If a legacy-store conflict appears, choose the existing or new store with
   an explicit absolute `--dir` on each relevant MCP and hook command.
   `init --store-only --dir <path>` prepares the selected destination but
   does not copy or validate an old database.
3. Keep the hook's derived project namespace separate from the MCP call's
   explicit `agent_id`. Search the former ID deliberately if an older hook
   used a subdirectory name. Reverting the routing code does not move facts.

These rules apply to MCP stdio and Claude hooks. HTTP serves the selected
service store and requires IDs per call. Other CLI commands retain their
documented working-directory behavior.

# Claude Code with a user-scoped MCP server

These launchers prepare the current Git checkout or worktree with
`graymatter init --store-only --quiet`, then start Claude Code from its root.
They are for a setup that already has GrayMatter available to Claude. Copy the
script for your platform to a directory on `PATH`, use the name `gm-claude`,
and run it from inside the checkout you want to use. Pass Claude's arguments
after the script name; they are forwarded unchanged.

## One-time setup

- Install a GrayMatter build that supports `init --store-only`, Git, and Claude
  Code. Existing global instructions and MCP registration can be kept.
- If Claude's MCP server is not yet registered at user scope, register it once:

  ```sh
  claude mcp add --scope user --transport stdio graymatter -- graymatter mcp serve
  ```

- Install global memory instructions if needed. `graymatter init --global`
  adds home-scoped instructions for Claude Code and OpenCode **and** performs
  normal setup in the directory where it runs. It does not register Claude's
  MCP server at user scope.
- Global Claude Code hooks are optional. Install them with
  `graymatter hooks install --scope global`; their guard skips unprepared
  directories.

The Bash script requires Bash and native `git`, `graymatter`, and `claude`
commands on `PATH`. Make it executable after copying. The PowerShell script
requires Windows, `pwsh` 7.3 or later, and native `git.exe`,
`graymatter.exe`, and `claude.exe` applications on `PATH`. Run the PowerShell
file as a script, not with dot-sourcing. Windows PowerShell 5.1 and `.cmd`,
`.bat`, or `.ps1` launchers for these commands are outside its contract.

## Usage

From the desired checkout or worktree:

```sh
gm-claude
```

On Windows, invoke the copied `gm-claude.ps1` with `pwsh` or an appropriate
PowerShell script association. Both scripts find the Git root, prepare its
default `.graymatter` directory, and launch Claude there. From outside Git,
they exit with code 2. If preparation fails, Claude does not start. Once
Claude starts, its exit code is returned.

The selected checkout or worktree must stay the session's root for this
workflow. If a Claude option changes the root or creates a worktree, enter
that destination first and run the launcher there. The MCP process and hooks
must also use the prepared root. These examples use the default relative
store; for a custom `--dir` path, configure **both** MCP and each hook with
that same absolute path. A single fixed global path shares one store across
projects.

`--store-only` creates `MEMORY.md` when the store directory has no marker or
database. It leaves an existing regular `gray.db` alone and does not add a
marker just for it. The command does not install client configuration,
instructions, hooks, or alter `PATH`; it does not verify database health or
remove files from earlier setup. A user-scoped MCP server can itself create
`gray.db` when it starts, even without this launcher.

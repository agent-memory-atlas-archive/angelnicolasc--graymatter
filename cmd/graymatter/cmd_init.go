package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/daemon"
)

type initCLIFlags struct {
	interactive, global, skipCodex, skipOpencode, skipClaudeCode, skipCursor bool
	withAntigravity, skipInstructions, noPath, enableKG, installHooks        bool
	storeOnly, replaceMCP, bestEffort                                        bool
	only                                                                     string
}

func initCmd() *cobra.Command {
	var f initCLIFlags
	cmd := &cobra.Command{
		Use: "init", Short: "Prepare a GrayMatter store and configure selected MCP clients",
		Long: `Prepares the selected local store and configures MCP clients. Existing custom
GrayMatter entries are preserved by default. Use --replace-mcp with an explicit
--only selection to replace just those clients' GrayMatter entries. Setup
validates every selected file before writing. Partial application fails unless
--best-effort is explicit; rerun after correcting the reported failure.

--global does not replace the normal setup of the current project. It also
writes home-scoped instructions. Project-scoped MCP configs remain per project
and require init or manual configuration there. Codex is the exception because
its MCP config is home-scoped. --skip-instructions affects project instructions;
explicit --global continues to write home-scoped instructions. The result
describes preparation, not whether a live MCP host loaded the configuration.

On Windows, non-interactive init requests a user PATH update by default.
Interactive init asks separately, defaulting to No. --no-path prevents both.
Updating an existing setup file, including instructions, requires permission
to read its audit SACL. Without it, preflight returns unsupported_metadata
before modifying the file. Edit it manually, skip that action when possible
(for example, --skip-instructions), or run with the metadata permission.

--store-only preserves its separate contract: it prepares only the selected
store marker, without clients, hooks, instructions, runtime, or PATH.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if f.storeOnly {
				if f.replaceMCP || f.bestEffort {
					return fmt.Errorf("--replace-mcp and --best-effort cannot be used with init --store-only")
				}
				return runStoreOnlyInit(cmd, args, storeOnlyInitOptions{
					dataDir: dataDir, global: f.global, hooks: f.installHooks,
					interactive: f.interactive, withAntigravity: f.withAntigravity,
					kg: f.enableKG, onlyChanged: cmd.Flags().Changed("only"),
					quiet: quiet, json: jsonOut,
				})
			}
			return runSetupInit(cmd, args, f)
		},
	}
	cmd.Flags().BoolVarP(&f.interactive, "interactive", "i", false, "interactive setup wizard")
	cmd.Flags().BoolVar(&f.storeOnly, "store-only", false, "prepare only the selected store; no client, hook, instruction, or PATH changes")
	cmd.Flags().BoolVar(&f.skipClaudeCode, "skip-claudecode", false, "do not configure .mcp.json")
	cmd.Flags().BoolVar(&f.skipCursor, "skip-cursor", false, "do not configure .cursor/mcp.json")
	cmd.Flags().BoolVar(&f.skipCodex, "skip-codex", false, "do not configure ~/.codex/config.toml")
	cmd.Flags().BoolVar(&f.skipOpencode, "skip-opencode", false, "do not configure opencode.jsonc")
	cmd.Flags().BoolVar(&f.withAntigravity, "with-antigravity", false, "also configure Antigravity")
	cmd.Flags().BoolVar(&f.skipInstructions, "skip-instructions", false, "skip project memory instructions; explicit --global still writes home instructions")
	cmd.Flags().BoolVar(&f.enableKG, "kg", false, "enable knowledge graph auto-population via "+daemon.KGSentinelFile)
	cmd.Flags().BoolVar(&f.installHooks, "hooks", false, "install Claude Code project memory hooks")
	cmd.Flags().BoolVar(&f.global, "global", false, "also install home-scoped instructions; current-project setup still runs and MCP configs remain per project")
	cmd.Flags().BoolVar(&f.noPath, "no-path", false, "do not add the executable directory to user PATH")
	cmd.Flags().BoolVar(&f.replaceMCP, "replace-mcp", false, "replace only selected GrayMatter MCP nodes (requires --only or wizard selection)")
	cmd.Flags().BoolVar(&f.bestEffort, "best-effort", false, "allow exit 0 for known partial apply with at least one successful required action")
	cmd.Flags().StringVar(&f.only, "only", "", "CSV of MCP clients (overrides skip flags): claudecode,cursor,codex,opencode,antigravity,windsurf,vscodecopilot")
	return cmd
}

func parseOnlyFlag(v string, agents []agentDef) (map[string]bool, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	known := make(map[string]bool, len(agents))
	valid := make([]string, 0, len(agents))
	for _, a := range agents {
		known[a.id] = true
		valid = append(valid, a.id)
	}
	out := map[string]bool{}
	var unknown []string
	for _, p := range strings.Split(v, ",") {
		p = strings.TrimSpace(strings.ToLower(p))
		if p == "" {
			continue
		}
		if !known[p] {
			unknown = append(unknown, p)
			continue
		}
		out[p] = true
	}
	if len(unknown) > 0 {
		return nil, fmt.Errorf("--only: unknown agent %s (valid: %s)", strings.Join(unknown, ", "), strings.Join(valid, ", "))
	}
	return out, nil
}

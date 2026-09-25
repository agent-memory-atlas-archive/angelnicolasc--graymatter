package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// testStdinReader is overridden in tests to provide canned input.
// Nil in production.
var testStdinReader *strings.Reader

func stdinReader() *bufio.Scanner {
	if testStdinReader != nil {
		return bufio.NewScanner(testStdinReader)
	}
	return bufio.NewScanner(os.Stdin)
}

// agentDef describes one known MCP agent.
//
// This table is the single source of truth for "which agent needs which file".
// It used to feed only the interactive wizard, while `init`'s flag path and
// `doctor` each carried their own parallel list. That drift is what let
// `--only opencode` keep writing CLAUDE.md (issue #13) and what let `doctor`
// report missing instructions for a project covered by `init --global`
// (issue #17): three lists, one of them updated.
type agentDef struct {
	id              string // claudecode, cursor, opencode, codex, antigravity
	name            string // display name
	configDesc      string // human-readable config-file description
	instructionFile string // project instruction file it reads ("" if none)
	optIn           bool   // wired only when explicitly asked for
	run             func() (writeResult, error)

	// configPath resolves the MCP config this agent actually reads, so doctor
	// looks exactly where the writer wrote.
	configPath func(projectDir string) (string, error)

	// globalInstruction is the home-scoped instruction file this agent reads in
	// every project, or nil when the agent has none that `init --global`
	// writes. Only these agents can be covered by a global install; for the
	// rest a project file is the only delivery path, and doctor must not credit
	// them for a global block they never read.
	globalInstruction func() (string, error)
}

// knownAgents returns the list of known agents, with project-scoped writers
// bound to the given project directory.
func knownAgents(projectDir string) []agentDef {
	return []agentDef{
		{
			id: "claudecode", name: "Claude Code", configDesc: ".mcp.json",
			// Claude Code reads CLAUDE.md and explicitly does not read
			// AGENTS.md; its user-scope file is ~/.claude/CLAUDE.md.
			instructionFile: "CLAUDE.md",
			run:             func() (writeResult, error) { return writeClaudeCodeProject(projectDir) },
			configPath: func(dir string) (string, error) {
				return filepath.Join(dir, ".mcp.json"), nil
			},
			globalInstruction: func() (string, error) {
				home, err := resolveHome()
				if err != nil {
					return "", err
				}
				return filepath.Join(home, ".claude", "CLAUDE.md"), nil
			},
		},
		{
			id: "cursor", name: "Cursor", configDesc: ".cursor/mcp.json",
			instructionFile: "AGENTS.md",
			run:             func() (writeResult, error) { return writeCursorProject(projectDir) },
			configPath: func(dir string) (string, error) {
				return filepath.Join(dir, ".cursor", "mcp.json"), nil
			},
		},
		{
			id: "opencode", name: "OpenCode", configDesc: "opencode.jsonc",
			instructionFile: "AGENTS.md",
			run:             func() (writeResult, error) { return writeOpencodeProject(projectDir) },
			configPath: func(dir string) (string, error) {
				return filepath.Join(dir, "opencode.jsonc"), nil
			},
			globalInstruction: func() (string, error) {
				d, err := opencodeConfigDir()
				if err != nil {
					return "", err
				}
				return filepath.Join(d, "AGENTS.md"), nil
			},
		},
		{
			id: "codex", name: "Codex", configDesc: "~/.codex/config.toml",
			instructionFile: "AGENTS.md",
			run:             writeCodexHome,
			configPath:      func(string) (string, error) { return codexConfigPath() },
		},
		{
			id: "antigravity", name: "Antigravity", configDesc: "mcp_config.json",
			// Antigravity reads AGENTS.md (project root cross-tool standard)
			// alongside its own GEMINI.md. AGENTS.md is sufficient here — see
			// https://antigravity.codes/blog/antigravity-agents-md-guide
			instructionFile: "AGENTS.md",
			optIn:           true,
			run:             func() (writeResult, error) { return writeAntigravityProject(projectDir) },
			configPath: func(dir string) (string, error) {
				return filepath.Join(dir, "mcp_config.json"), nil
			},
		},
		{
			id: "windsurf", name: "Windsurf", configDesc: ".windsurf/mcp.json",
			instructionFile: "",
			run:             func() (writeResult, error) { return writeWindsurfProject(projectDir) },
			configPath: func(dir string) (string, error) {
				return filepath.Join(dir, ".windsurf", "mcp.json"), nil
			},
		},
		{
			id: "vscodecopilot", name: "VS Code Copilot Agent", configDesc: ".vscode/mcp.json",
			instructionFile: "",
			run:             func() (writeResult, error) { return writeVSCodeCopilotProject(projectDir) },
			configPath: func(dir string) (string, error) {
				return filepath.Join(dir, ".vscode", "mcp.json"), nil
			},
		},
	}
}

// instructionFilesFor returns the project instruction files the selected agents
// need, deduped, in knownAgents order. An OpenCode-only selection yields just
// AGENTS.md; a Claude-Code-only selection just CLAUDE.md.
func instructionFilesFor(agents []agentDef, selected map[string]bool) []string {
	var out []string
	seen := map[string]bool{}
	for _, a := range agents {
		if !selected[a.id] || a.instructionFile == "" || seen[a.instructionFile] {
			continue
		}
		seen[a.instructionFile] = true
		out = append(out, a.instructionFile)
	}
	return out
}

// runInteractiveWizard runs the interactive init wizard.
// It asks which agents to wire, then creates only the files needed for those.
// projectDir is the project root; writers and instruction files go there.
func runInteractiveWizard(dir, projectDir string, quiet bool) error {
	selection, err := resolveStoreSelection(dir, false, projectDir)
	if err != nil {
		return err
	}
	selected, pathRequested, err := collectInitWizardSelection(knownAgents(projectDir), false, false, false, false)
	if err != nil {
		return err
	}
	opts := initOptions{selection: selection, projectDir: projectDir, selected: selected, interactive: true, path: pathRequested, quiet: quiet}
	result := newSetupResult(false)
	if err := planInit(opts, &result); err != nil {
		closeInitPlans(&result)
		return err
	}
	applyInit(opts, &result)
	closeInitPlans(&result)
	result.finalize()
	if !result.Complete {
		return fmt.Errorf("initialization incomplete")
	}
	return nil
}

// maybeAddToPath applies the Windows PATH change when refusable-setup allows
// it, reports the fact without any restart wording (the single authoritative
// restart instruction lives in printNextSteps), and returns whether the PATH
// was actually modified.
var initAddExeDirToUserPath = addExeDirToUserPath

func maybeAddToPath(quiet bool) bool {
	added, pathErr := initAddExeDirToUserPath()
	if pathErr != nil {
		if !quiet {
			exe, _ := os.Executable()
			fmt.Fprintf(os.Stderr,
				"\n  Warning: could not add %s to PATH: %v\n  Add it manually so you can type 'graymatter' from any directory.\n",
				filepath.Dir(exe), pathErr)
		}
		return false
	}
	if added && !quiet {
		exe, _ := os.Executable()
		fmt.Printf("  Added %s to your user PATH\n", filepath.Dir(exe))
	}
	return added
}

// askForAgents prints the interactive menu and returns the selected agent IDs.
// Returns an empty slice if the user selects "None" or enters nothing.
func askForAgents(agents []agentDef) []string {
	fmt.Println()
	fmt.Println("Which AI agent software do you use?")
	fmt.Println()
	fmt.Println("Select all that apply (enter numbers separated by commas or spaces):")
	fmt.Println()
	fmt.Println("  0  None — data directory only")
	for i, a := range agents {
		fmt.Printf("  %d  %s (%s)\n", i+1, a.name, a.configDesc)
	}
	fmt.Println()
	fmt.Print("Selection: ")

	scanner := stdinReader()
	if !scanner.Scan() {
		return nil
	}
	input := strings.TrimSpace(scanner.Text())
	if input == "" {
		return nil
	}

	// Tokenize by comma, space, or tab.
	parts := strings.FieldsFunc(input, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\t'
	})

	seen := make(map[string]bool)
	var result []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err != nil {
			continue
		}
		if n == 0 {
			return nil // "None" selected — clears all other selections
		}
		if n < 1 || n > len(agents) {
			continue // out of range — silently skip
		}
		id := agents[n-1].id
		if !seen[id] {
			seen[id] = true
			result = append(result, id)
		}
	}
	return result
}

package main

import (
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
)

func parseWizardSelection(line string, agents []agentDef) (map[string]bool, bool) {
	selected := map[string]bool{}
	line = strings.TrimSpace(line)
	if line == "" || line == "0" {
		return selected, true
	}
	parts := strings.FieldsFunc(line, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	if len(parts) == 0 {
		return selected, false
	}
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 1 || n > len(agents) {
			return nil, false
		}
		selected[agents[n-1].id] = true
	}
	return selected, true
}

func collectInitWizardSelection(agents []agentDef, noPath, global, hooks, kg bool) (map[string]bool, bool, error) {
	scanner := stdinReader()
	for {
		fmt.Println("\nWhich MCP clients should be configured?")
		fmt.Println("  0  None — no MCP clients")
		for i, a := range agents {
			fmt.Printf("  %d  %s (%s)\n", i+1, a.name, a.configDesc)
		}
		fmt.Print("Selection: ")
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return nil, false, err
			}
			return nil, false, errors.New("selection_cancelled")
		}
		selected, valid := parseWizardSelection(scanner.Text(), agents)
		if !valid {
			fmt.Println("Invalid selection; enter client numbers or 0 alone.")
			continue
		}
		fmt.Printf("Selected %d MCP client(s).", len(selected))
		if global {
			fmt.Print(" Global instructions requested.")
		}
		if hooks {
			fmt.Print(" Claude Code hooks requested.")
		}
		if kg {
			fmt.Print(" Knowledge graph requested.")
		}
		fmt.Println()
		if noPath || runtime.GOOS != "windows" {
			return selected, false, nil
		}
		for {
			fmt.Print("Add GrayMatter to your user PATH? [y/N]: ")
			if !scanner.Scan() {
				if err := scanner.Err(); err != nil {
					return nil, false, err
				}
				return selected, false, nil
			}
			switch strings.ToLower(strings.TrimSpace(scanner.Text())) {
			case "", "n", "no":
				return selected, false, nil
			case "y", "yes":
				return selected, true, nil
			default:
				fmt.Println("Enter yes or no.")
			}
		}
	}
}

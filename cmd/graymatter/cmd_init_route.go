package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// storeSelection is captured once, before any init action. Explicitness is a
// property of the CLI invocation, not of the spelling of the selected path.
type storeSelection struct {
	Path     string
	Explicit bool
}

func resolveStoreSelection(configuredDir string, dirChanged bool, capturedCWD string) (storeSelection, error) {
	if strings.TrimSpace(configuredDir) == "" {
		return storeSelection{}, fmt.Errorf("--dir must not be empty")
	}
	path := configuredDir
	if !filepath.IsAbs(path) {
		if capturedCWD == "" || !filepath.IsAbs(capturedCWD) {
			return storeSelection{}, fmt.Errorf("cannot resolve relative data directory without an absolute working directory")
		}
		path = filepath.Join(capturedCWD, path)
	}
	return storeSelection{Path: filepath.Clean(path), Explicit: dirChanged}, nil
}

func mcpServeArgs(selection storeSelection) []string {
	args := []string{"mcp", "serve"}
	if selection.Explicit {
		args = append(args, "--dir", selection.Path)
	}
	return args
}

func hookRunArgs(selection storeSelection, event string) []string {
	args := []string{"hooks", "run", event, hooksCommandMarker}
	if selection.Explicit {
		args = append(args, "--dir", selection.Path)
	}
	return args
}

package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

type storeOnlyInitOptions struct {
	dataDir         string
	global          bool
	hooks           bool
	interactive     bool
	withAntigravity bool
	kg              bool
	onlyChanged     bool
	quiet           bool
	json            bool
}

type storeOnlyInitResult struct {
	SchemaVersion   int    `json:"schema_version"`
	Mode            string `json:"mode"`
	DataDir         string `json:"data_dir"`
	Status          string `json:"status"`
	Marker          string `json:"marker"`
	RuntimeVerified bool   `json:"runtime_verified"`
}

func validateStoreOnlyInit(args []string, opts storeOnlyInitOptions) (string, error) {
	if len(args) != 0 {
		return "", fmt.Errorf("init --store-only accepts no positional arguments")
	}
	for _, flag := range []struct {
		name    string
		enabled bool
	}{
		{"--global", opts.global},
		{"--hooks", opts.hooks},
		{"--interactive", opts.interactive},
		{"--with-antigravity", opts.withAntigravity},
		{"--kg", opts.kg},
		{"--only", opts.onlyChanged},
	} {
		if flag.enabled {
			return "", fmt.Errorf("%s cannot be used with init --store-only", flag.name)
		}
	}
	if strings.TrimSpace(opts.dataDir) == "" {
		return "", fmt.Errorf("--dir must not be empty with init --store-only")
	}
	path, err := filepath.Abs(opts.dataDir)
	if err != nil {
		return "", fmt.Errorf("resolve data dir: %w", err)
	}
	return path, nil
}

func runStoreOnlyInit(cmd *cobra.Command, args []string, opts storeOnlyInitOptions) error {
	path, err := validateStoreOnlyInit(args, opts)
	if err != nil {
		return err
	}
	result, err := prepareStoreDirectory(path)
	if err != nil {
		return err
	}
	return renderStoreOnlyInitResult(cmd, storeOnlyInitResult{
		SchemaVersion: 1, Mode: "store-only", DataDir: path,
		Status: result.status, Marker: result.marker, RuntimeVerified: false,
	}, opts)
}

func renderStoreOnlyInitResult(cmd *cobra.Command, result storeOnlyInitResult, opts storeOnlyInitOptions) error {
	if opts.json {
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
			return fmt.Errorf("write store-only JSON result: %w", err)
		}
		return nil
	}
	if opts.quiet {
		return nil
	}
	if result.Status == "created" {
		_, err := fmt.Fprintf(cmd.OutOrStdout(), "GrayMatter store prepared at %s (created MEMORY.md).\n", result.DataDir)
		if err != nil {
			return fmt.Errorf("write store-only result: %w", err)
		}
		return nil
	}
	_, err := fmt.Fprintf(cmd.OutOrStdout(), "GrayMatter store already prepared at %s (%s).\n", result.DataDir, result.Marker)
	if err != nil {
		return fmt.Errorf("write store-only result: %w", err)
	}
	return nil
}

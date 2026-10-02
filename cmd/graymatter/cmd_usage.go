package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/angelnicolasc/graymatter/cmd/graymatter/internal/usage"
	"github.com/spf13/cobra"
)

func usageCmd() *cobra.Command {
	var stateDir string
	var outputJSON bool
	root := &cobra.Command{Use: "usage", Short: "Account limits, billing observations and local usage", Long: "Read opt-in provider reports and imported telemetry. No credentials are discovered automatically. Account state defaults to the user configuration directory; use --state-dir with --dir's value to read project harness events."}
	root.PersistentFlags().StringVar(&stateDir, "state-dir", "", "Usage state root (default: user config directory/graymatter)")
	root.PersistentFlags().BoolVar(&outputJSON, "json", false, "Print JSON")
	state := func() (string, error) {
		if stateDir != "" {
			return stateDir, nil
		}
		return usage.DefaultStateDir()
	}
	var refresh bool
	show := &cobra.Command{Use: "show", Short: "Show saved observations; optionally refresh configured connections", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dir, e := state()
		if e != nil {
			return e
		}
		s, loadErr := usage.Load(cmd.Context(), dir, refresh)
		if outputJSON || jsonOut {
			return errors.Join(loadErr, json.NewEncoder(cmd.OutOrStdout()).Encode(s))
		}
		var out bytes.Buffer
		if len(s.Quotas)+len(s.Costs)+len(s.Contexts)+len(s.Events) == 0 {
			fmt.Fprintln(&out, "No usage observations. Use usage connect, usage statusline, or usage import.")
		}
		for _, q := range s.Quotas {
			value := "unavailable"
			if q.UsedPercent != nil {
				value = fmt.Sprintf("%.1f%%", *q.UsedPercent)
			}
			fresh := ""
			if q.Stale {
				fresh = " (stale)"
			}
			reset := ""
			if q.ResetAt != nil {
				reset = " reset " + q.ResetAt.Format(time.RFC3339)
			}
			fmt.Fprintf(&out, "%s %s %s: %s%s%s\n", q.Provider, q.AccountID, q.Window, value, fresh, reset)
		}
		for _, c := range usage.EffectiveCosts(s.Costs) {
			fmt.Fprintf(&out, "%s %s: %s %s (%s, %s, %s – %s, partial=%t, stale=%t)\n", c.Provider, c.AccountID, c.Amount, c.Currency, c.Kind, c.Scope, c.StartAt.Format(time.RFC3339), c.EndAt.Format(time.RFC3339), c.Partial, c.Stale)
		}
		for _, x := range s.Contexts {
			used, limit := "unknown", "unknown"
			if x.UsedTokens != nil {
				used = fmt.Sprint(*x.UsedTokens)
			}
			if x.LimitTokens != nil {
				limit = fmt.Sprint(*x.LimitTokens)
			}
			fmt.Fprintf(&out, "context %s %s session %s: %s / %s tokens (estimated=%t, stale=%t)\n", x.Provider, x.AccountID, x.SessionID, used, limit, x.Estimated, x.Stale)
			if x.ComponentDelta != nil {
				fmt.Fprintf(&out, "  Component estimates differ from observed occupancy by %+d tokens.\n", *x.ComponentDelta)
			}
		}
		if len(s.Events) > 0 {
			fmt.Fprintf(&out, "%d deduplicated request observations (local/imported coverage only).\n", len(s.Events))
		}
		for _, c := range s.Connections {
			fmt.Fprintf(&out, "connection %s: %s %s\n", c.ID, c.State, c.Error)
		}
		for _, warning := range s.Warnings {
			fmt.Fprintf(&out, "warning: %s\n", warning)
		}
		_, writeErr := io.Copy(cmd.OutOrStdout(), &out)
		return errors.Join(loadErr, writeErr)
	}}
	show.Flags().BoolVar(&refresh, "refresh", false, "Refresh explicitly configured enabled connections")
	var importFile, pricesFile string
	imp := &cobra.Command{Use: "import", Short: "Import a version 1 usage JSON snapshot from a file or stdin", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dir, e := state()
		if e != nil {
			return e
		}
		r, close, e := usageInput(cmd.InOrStdin(), importFile)
		if e != nil {
			return e
		}
		defer close()
		s, e := usage.DecodeSnapshot(r)
		if e != nil {
			return e
		}
		if pricesFile != "" {
			p, close, e := usageInput(cmd.InOrStdin(), pricesFile)
			if e != nil {
				return e
			}
			defer close()
			prices, e := usage.DecodePriceBook(p)
			if e != nil {
				return e
			}
			for _, event := range s.Events {
				cost, e := usage.Estimate(event, prices)
				if e != nil {
					return e
				}
				s.Costs = append(s.Costs, cost)
			}
		}
		if e = usage.Import(cmd.Context(), dir, s); e != nil {
			return e
		}
		_, e = fmt.Fprintln(cmd.OutOrStdout(), "Usage observations imported.")
		return e
	}}
	imp.Flags().StringVar(&importFile, "file", "-", "JSON snapshot path, or - for stdin")
	imp.Flags().StringVar(&pricesFile, "prices", "", "Optional explicit price-book JSON to estimate imported events")
	var account string
	statusline := &cobra.Command{Use: "statusline", Short: "Import Claude Code statusline JSON from stdin", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dir, e := state()
		if e != nil {
			return e
		}
		s, e := usage.ParseClaudeStatusline(cmd.InOrStdin(), account, time.Now().UTC())
		if e != nil {
			return e
		}
		return usage.Import(cmd.Context(), dir, s)
	}}
	statusline.Flags().StringVar(&account, "account", "", "Stable account label (required)")
	_ = statusline.MarkFlagRequired("account")
	var kind, connectionAccount, keyEnv, command string
	var args []string
	var disabled bool
	connect := &cobra.Command{Use: "connect NAME", Short: "Explicitly configure an account connection (does not connect until refresh)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, names []string) error {
		dir, e := state()
		if e != nil {
			return e
		}
		c, e := usage.ReadConfig(dir)
		if e != nil {
			return e
		}
		v := usage.Connection{ID: names[0], Kind: kind, AccountID: connectionAccount, APIKeyEnv: keyEnv, Enabled: !disabled}
		switch kind {
		case "codex-app-server", "openai-costs":
			v.Provider = "openai"
		case "anthropic-costs":
			v.Provider = "anthropic"
		}
		if command != "" {
			v.Command = append([]string{command}, args...)
		}
		found := false
		for i := range c.Connections {
			if c.Connections[i].ID == v.ID {
				c.Connections[i] = v
				found = true
			}
		}
		if !found {
			c.Connections = append(c.Connections, v)
		}
		if e = usage.SaveConfig(dir, c); e != nil {
			return e
		}
		_, e = fmt.Fprintln(cmd.OutOrStdout(), "Connection saved. Run usage show --refresh to read it.")
		return e
	}}
	connect.Flags().StringVar(&kind, "kind", "", "codex-app-server, openai-costs, or anthropic-costs")
	connect.Flags().StringVar(&connectionAccount, "account", "", "Stable account or organization label")
	connect.Flags().StringVar(&keyEnv, "key-env", "", "Admin key environment variable name, never the key itself")
	connect.Flags().StringVar(&command, "command", "", "Explicit executable path for Codex app-server")
	connect.Flags().StringArrayVar(&args, "arg", nil, "Executable argument; repeat as needed")
	connect.Flags().BoolVar(&disabled, "disabled", false, "Save connection disabled")
	_ = connect.MarkFlagRequired("kind")
	_ = connect.MarkFlagRequired("account")
	config := &cobra.Command{Use: "config", Short: "Show local connection configuration (contains environment names only)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		dir, e := state()
		if e != nil {
			return e
		}
		c, e := usage.ReadConfig(dir)
		if e != nil {
			return e
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(c)
	}}
	root.AddCommand(show, imp, statusline, connect, config)
	root.SilenceUsage = true
	for _, c := range root.Commands() {
		c.SilenceUsage = true
	}
	return root
}

func usageInput(stdin io.Reader, path string) (io.Reader, func(), error) {
	if path == "" || path == "-" {
		return stdin, func() {}, nil
	}
	f, e := os.Open(path)
	if e != nil {
		return nil, func() {}, e
	}
	return f, func() { _ = f.Close() }, nil
}

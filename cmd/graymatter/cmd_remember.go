package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/angelnicolasc/graymatter/pkg/memory"
	"github.com/angelnicolasc/graymatter/pkg/memory/rpc"
	"github.com/spf13/cobra"
)

func rememberCmd() *cobra.Command {
	var shared bool
	var confidence string

	cmd := &cobra.Command{
		Use:   "remember <agent-id> <text>",
		Short: "Store a fact for an agent",
		Example: `  graymatter remember "sales-closer" "Maria didn't reply Wednesday. Third touchpoint due Friday."
  graymatter remember "code-reviewer" "Always check for nil pointer dereferences in Go code."
  graymatter remember --shared "Global preference: always use bullet points."`,
		Args: func(cmd *cobra.Command, args []string) error {
			if shared && len(args) == 1 {
				return nil
			}
			return cobra.ExactArgs(2)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			opts, err := cliWriteOptions(cmd, confidence)
			if err != nil {
				return err
			}
			agentID, text := memory.SharedAgentID, args[len(args)-1]
			if len(args) == 2 {
				agentID = args[0]
			}
			store, err := openStore()
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if opts.Confidence != nil {
				var fact memory.Fact
				if shared {
					agentID = memory.SharedAgentID
					writer, ok := store.(rpc.SharedConfidenceWriter)
					if !ok {
						return memory.ErrConfidenceUnsupported
					}
					fact, err = writer.PutSharedWithOptionsReturningFact(ctx, text, opts)
				} else {
					writer, ok := store.(rpc.ConfidenceWriter)
					if !ok {
						return memory.ErrConfidenceUnsupported
					}
					fact, err = writer.PutWithOptionsReturningFact(ctx, agentID, text, opts)
				}
				if err != nil {
					if fact.ID != "" {
						return fmt.Errorf("remember failed after committing fact_id %s (confidence %s): %w", fact.ID, memory.EffectiveConfidence(fact.Confidence), err)
					}
					return err
				}
				if jsonOut {
					data, _ := json.Marshal(map[string]any{"agent_id": agentID, "status": "stored", "fact_id": fact.ID, "confidence": fact.Confidence})
					fmt.Println(string(data))
				} else if !quiet {
					fmt.Printf("Remembered: [%s] %s (confidence %s; fact_id %s)\n", agentID, text, fact.Confidence, fact.ID)
				}
				return nil
			}

			if shared {
				if err := store.PutShared(ctx, text); err != nil {
					return err
				}
				if jsonOut {
					data, _ := json.Marshal(map[string]string{"scope": "shared", "status": "stored"})
					fmt.Println(string(data))
				} else if !quiet {
					fmt.Printf("Remembered (shared): %s\n", text)
				}
				return nil
			}

			if err := store.Remember(ctx, agentID, text); err != nil {
				return err
			}

			if jsonOut {
				data, _ := json.Marshal(map[string]string{"agent_id": agentID, "status": "stored"})
				fmt.Println(string(data))
			} else if !quiet {
				fmt.Printf("Remembered: [%s] %s\n", agentID, text)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&shared, "shared", false, "store in shared memory (readable by all agents)")
	cmd.Flags().StringVar(&confidence, "confidence", "", "declared confidence: verified, inferred or unverified (omitted: legacy inferred)")
	return cmd
}

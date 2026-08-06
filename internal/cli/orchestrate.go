package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Gere2/neurofs/internal/gamestate"
	"github.com/Gere2/neurofs/internal/orchestrator"
	"github.com/spf13/cobra"
)

func newOrchestrateCmd() *cobra.Command {
	var (
		repoPath string
		jsonOut  bool
	)

	cmd := &cobra.Command{
		Use:   "orchestrate <question>",
		Short: "Decompose a question, route to optimal models, execute and verify",
		Long: `Orchestrate breaks down a question into logical sub-tasks, assigns each
sub-task to the most cost-effective model (Claude, Gemini, GPT), fetches targeted
codebase context via NeuroFS chunk search, and verifies response grounding.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			question := args[0]

			orc, err := orchestrator.NewOrchestrator(repoPath, nil)
			if err != nil {
				return fmt.Errorf("orchestrate: init failed: %w", err)
			}

			if !jsonOut {
				fmt.Fprintf(os.Stderr, "NeuroFS Orchestrator — decomposing %q\n\n", question)
			}

			result, err := orc.Run(context.Background(), orchestrator.OrchestrationOptions{
				Question: question,
				RepoRoot: repoPath,
				Callback: func(ev orchestrator.StatusEvent) {
					if !jsonOut {
						switch ev.Status {
						case orchestrator.StatusRunning:
							fmt.Fprintf(os.Stderr, "  [⏳ RUNNING] %s (%s / %s)\n", ev.TaskID, ev.Model, ev.Provider)
						case orchestrator.StatusDone:
							fmt.Fprintf(os.Stderr, "  [✓ DONE]    %s (tokens: %d in / %d out, cost: $%.4f, grounding: %.1f%%)\n",
								ev.TaskID, ev.InputTokens, ev.OutputTokens, ev.CostUSD, ev.Grounding*100)
						case orchestrator.StatusFailed:
							fmt.Fprintf(os.Stderr, "  [✗ FAILED]  %s: %s\n", ev.TaskID, ev.Error)
						case orchestrator.StatusSkipped:
							fmt.Fprintf(os.Stderr, "  [⊘ SKIPPED] %s: %s\n", ev.TaskID, ev.Error)
						}
					}
				},
			})

			if err != nil {
				return fmt.Errorf("orchestrate execution error: %w", err)
			}

			// Update gamestate & log tournament records
			if home, err := os.UserHomeDir(); err == nil {
				dir := filepath.Join(home, ".neurofs")
				tLogger := orchestrator.NewTournamentLogger(dir)
				if ps, err := gamestate.Load(dir); err == nil {
					for _, t := range result.Plan.Tasks {
						if t.Status == orchestrator.StatusDone {
							ps.RecordTaskResult(t.Model, t.Grounding, t.CostUSD, t.CascadeLevel, t.CascadeSaved, t.Complexity == orchestrator.Complex, 0.85)
							ps.GrantXPForTask(t.Grounding, t.CascadeLevel, t.CascadeSaved, t.Complexity == orchestrator.Complex, 0.85)
							_ = tLogger.LogRecord(orchestrator.TournamentRecord{
								PlanID:       result.Plan.ID,
								TaskID:       t.ID,
								Kind:         t.Kind,
								Complexity:   t.Complexity,
								Model:        t.Model,
								Provider:     t.Provider,
								Grounding:    t.Grounding,
								CostUSD:      t.CostUSD,
								DurationMs:   t.Duration().Milliseconds(),
								CascadeLevel: t.CascadeLevel,
								Accepted:     true,
							})
						}
					}
					ps.CheckAchievements()
					_ = ps.Save(dir)
				}
			}

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(result)
			}

			fmt.Fprintf(os.Stderr, "\n  Plan Execution Summary:\n")
			fmt.Fprintf(os.Stderr, "    tasks       : %d total\n", len(result.Plan.Tasks))
			fmt.Fprintf(os.Stderr, "    total cost  : $%.4f USD\n", result.TotalCostUSD)
			fmt.Fprintf(os.Stderr, "    grounding   : %.1f%%\n", result.MeanGrounding*100)
			fmt.Fprintf(os.Stderr, "    duration    : %dms\n\n", result.DurationMs)

			for i, t := range result.Plan.Tasks {
				fmt.Printf("--- Sub-Task %d [%s] (%s / %s) ---\n", i+1, t.ID, t.Kind, t.Model)
				fmt.Printf("Description: %s\n\n", t.Description)
				if t.Response != "" {
					fmt.Printf("%s\n\n", t.Response)
				} else if t.Error != "" {
					fmt.Printf("Error: %s\n\n", t.Error)
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", ".", "Path to repository root")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output machine-readable JSON")

	return cmd
}

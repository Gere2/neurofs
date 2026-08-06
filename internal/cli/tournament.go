package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Gere2/neurofs/internal/orchestrator"
	"github.com/spf13/cobra"
)

func newTournamentCmd() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "tournament",
		Short: "Display War Room empirical model rankings and recommendations",
		Long:  "Analyze routing_history.jsonl and display win rates, mean grounding, cost efficiency, and recommended model routing rules per task kind.",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("tournament: user home dir: %w", err)
			}
			historyPath := filepath.Join(home, ".neurofs", "routing_history.jsonl")

			analysis, err := orchestrator.AnalyzeTournament(historyPath, 0.85)
			if err != nil {
				return fmt.Errorf("tournament: analysis failed: %w", err)
			}

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(analysis)
			}

			fmt.Fprintf(os.Stdout, "🏆 War Room Model Tournament Analysis (%d total runs):\n\n", analysis.TotalRecords)

			if len(analysis.ByKind) == 0 {
				fmt.Fprintf(os.Stdout, "  No execution records logged yet. Run `neurofs orchestrate` to populate empirical data.\n")
				return nil
			}

			for kind, perfList := range analysis.ByKind {
				rec := analysis.Recommendations[kind]
				fmt.Fprintf(os.Stdout, "  [ %s ]  Recommended: %s\n", kind, rec)
				for _, p := range perfList {
					fmt.Fprintf(os.Stdout, "    • %-16s  Runs: %-3d | WinRate: %3.0f%% | Grounding: %3.0f%% | Cost: $%.4f\n",
						p.Model, p.TotalRuns, p.WinRate*100, p.MeanGrounding*100, p.MeanCostUSD)
				}
				fmt.Fprintln(os.Stdout)
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output machine-readable JSON")
	return cmd
}

package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Gere2/neurofs/internal/gamestate"
	"github.com/spf13/cobra"
)

func newPlayerCmd() *cobra.Command {
	var jsonOut bool

	cmd := &cobra.Command{
		Use:   "player",
		Short: "Display Agent Commander progression, level, stats, and achievements",
		Long:  "Display current player level, title, XP progress, model squad statistics, and unlocked achievements.",
		RunE: func(cmd *cobra.Command, args []string) error {
			home, err := os.UserHomeDir()
			if err != nil {
				return fmt.Errorf("player: user home dir: %w", err)
			}
			dir := filepath.Join(home, ".neurofs")
			ps, err := gamestate.Load(dir)
			if err != nil {
				return fmt.Errorf("player: load state failed: %w", err)
			}

			ps.CheckAchievements()

			if jsonOut {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(ps)
			}

			_, _ = fmt.Fprintf(os.Stdout, "🛡️ Agent Commander Status:\n")
			_, _ = fmt.Fprintf(os.Stdout, "  Level       : Lv.%d (%s)\n", ps.Level, ps.Title)
			_, _ = fmt.Fprintf(os.Stdout, "  XP          : %d / %d\n", ps.XP, ps.XPToNext)
			_, _ = fmt.Fprintf(os.Stdout, "  Streak      : 🔥 %d days\n", ps.Streak)
			_, _ = fmt.Fprintf(os.Stdout, "  Missions    : %d completed\n", ps.TotalMissions)
			_, _ = fmt.Fprintf(os.Stdout, "  Grounding   : 🎯 %.1f%%\n", ps.MeanGrounding*100)
			_, _ = fmt.Fprintf(os.Stdout, "  Saved USD   : 💰 $%.4f\n\n", ps.TotalSavedUSD)

			_, _ = fmt.Fprintf(os.Stdout, "🤖 Model Squad Roster:\n")
			for _, a := range ps.Agents {
				_, _ = fmt.Fprintf(os.Stdout, "  %-12s %s  Wins: %-4d | Grounding: %3.0f%% | Cost: $%.4f | Cascade Avoided: %d\n",
					a.DisplayName, a.Emoji, a.Wins, a.Reliability*100, a.TotalCostUSD, a.CascadesAvoided)
			}

			if len(ps.Achievements) > 0 {
				_, _ = fmt.Fprintf(os.Stdout, "\n🎖️ Unlocked Achievements (%d):\n", len(ps.Achievements))
				for _, ach := range ps.Achievements {
					_, _ = fmt.Fprintf(os.Stdout, "  %s  %-20s — %s\n", ach.Emoji, ach.Name, ach.Description)
				}
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&jsonOut, "json", false, "Output machine-readable JSON")
	return cmd
}

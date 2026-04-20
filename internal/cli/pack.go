package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/output"
	"github.com/neuromfs/neuromfs/internal/packager"
	"github.com/neuromfs/neuromfs/internal/project"
	"github.com/neuromfs/neuromfs/internal/ranking"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/spf13/cobra"
)

func newPackCmd() *cobra.Command {
	var (
		budget       int
		repoPath     string
		format       string
		outPath      string
		forTarget    string
		focus        string
		changed      bool
		maxFiles     int
		maxFragments int
		saveBundle   string
	)

	cmd := &cobra.Command{
		Use:   "pack <query>",
		Short: "Export a context bundle to a file",
		Long: `Pack writes a ranked, budget-bounded context bundle to disk.

Use --for claude to emit a prompt-shaped output ready to paste into a fresh
Claude conversation. Combine with --focus to restrict the ranker to a
subtree, --changed to prioritise your working set, and --max-files /
--max-fragments to cap the bundle regardless of budget slack.

Examples:
  neurofs pack "why does ranking stem utility to util" --for claude \
      --focus internal/ranking --budget 3000 --out /tmp/ctx.prompt

  neurofs pack "review my edits" --for claude --changed --max-files 6 \
      --out /tmp/changed.prompt`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			query := args[0]

			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("pack: %w", err)
			}

			db, err := storage.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("pack: open index (did you run 'neurofs scan'?): %w", err)
			}
			defer db.Close()

			count, err := db.FileCount()
			if err != nil {
				return fmt.Errorf("pack: %w", err)
			}
			if count == 0 {
				return fmt.Errorf("pack: index is empty — run 'neurofs scan' first")
			}

			files, err := db.AllFiles()
			if err != nil {
				return fmt.Errorf("pack: load index: %w", err)
			}

			info := loadProjectInfo(db)
			rankOpts := ranking.Options{
				Project: info,
				Focus:   focus,
			}
			if changed {
				rankOpts.ChangedFiles = gitChangedFiles(cfg.RepoRoot)
				fmt.Fprintf(os.Stderr, "pack: --changed matched %d files from git\n", len(rankOpts.ChangedFiles))
			}

			ranked := ranking.RankWithOptions(files, query, rankOpts)

			// --for claude implies aggressive compression unless overridden:
			// prompts are cheaper when signatures replace full bodies.
			preferSig := output.Format(forTarget) == output.FormatClaude

			bundle, err := packager.Pack(ranked, query, packager.Options{
				Budget:           budget,
				MaxFiles:         maxFiles,
				MaxFragments:     maxFragments,
				PreferSignatures: preferSig,
			})
			if err != nil {
				return fmt.Errorf("pack: %w", err)
			}

			dest, err := os.Create(outPath)
			if err != nil {
				return fmt.Errorf("pack: create output file: %w", err)
			}
			defer dest.Close()

			if err := writeBundle(dest, bundle, format, forTarget, files, info); err != nil {
				return fmt.Errorf("pack: write: %w", err)
			}

			if saveBundle != "" {
				if err := writeBundleJSON(saveBundle, bundle); err != nil {
					return fmt.Errorf("pack: --save-bundle: %w", err)
				}
				fmt.Fprintf(os.Stderr, "  snapshot: %s\n", saveBundle)
			}

			fmt.Fprintf(os.Stderr, "NeuroFS — bundle saved to %s\n", outPath)
			fmt.Fprintf(os.Stderr, "  format  : %s\n", effectiveFormat(format, forTarget))
			fmt.Fprintf(os.Stderr, "  tokens  : %d / %d\n",
				bundle.Stats.TokensUsed, bundle.Stats.TokensBudget)
			fmt.Fprintf(os.Stderr, "  files   : %d included\n", bundle.Stats.FilesIncluded)
			if bundle.Stats.CompressionRatio > 0 {
				fmt.Fprintf(os.Stderr, "  ratio   : %.1fx compression\n",
					bundle.Stats.CompressionRatio)
			}

			return nil
		},
	}

	cmd.Flags().IntVar(&budget, "budget", config.DefaultBudget, "Token budget for the bundle")
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().StringVar(&format, "format", "markdown", "Output format: markdown | json | text | claude")
	cmd.Flags().StringVar(&forTarget, "for", "", "Shortcut for --format; 'claude' enables prompt-shaped output and aggressive compression")
	cmd.Flags().StringVar(&outPath, "out", "", "Output file path (required)")
	cmd.Flags().StringVar(&focus, "focus", "", "Boost files under these path prefixes (comma-separated)")
	cmd.Flags().BoolVar(&changed, "changed", false, "Boost files reported by git status (graceful no-op outside git repos)")
	cmd.Flags().IntVar(&maxFiles, "max-files", 0, "Hard cap on files included in the bundle (0 = no cap)")
	cmd.Flags().IntVar(&maxFragments, "max-fragments", 0, "Hard cap on fragments included in the bundle (0 = no cap)")
	cmd.Flags().StringVar(&saveBundle, "save-bundle", "", "Also write a JSON snapshot of the bundle here (consumed by 'audit replay --bundle')")
	_ = cmd.MarkFlagRequired("out")

	return cmd
}

// writeBundleJSON serialises the Bundle struct for later replay. Kept
// separate from the --format=json writer so callers can mix a
// human-readable --for claude output with a machine snapshot in one run.
func writeBundleJSON(path string, b models.Bundle) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// writeBundle dispatches to the correct serialiser. The Claude path takes a
// RepoSummary derived from the already-loaded index, so the prompt gets
// repo orientation for free without another DB query.
func writeBundle(dest *os.File, b models.Bundle, format, forTarget string, files []models.FileRecord, info *project.Info) error {
	eff := effectiveFormat(format, forTarget)
	if eff == string(output.FormatClaude) {
		return output.WriteClaude(dest, b, buildRepoSummary(files, info))
	}
	return output.Write(dest, b, output.Format(eff))
}

// effectiveFormat collapses --format and --for into a single value. --for
// wins when set, so `--for claude --format markdown` still produces Claude
// output (the shortcut is the user's stated intent).
func effectiveFormat(format, forTarget string) string {
	if forTarget != "" {
		return forTarget
	}
	if format == "" {
		return string(output.FormatMarkdown)
	}
	return format
}

// buildRepoSummary produces a compact orientation block from the in-memory
// file list. We intentionally keep it cheap — no new DB queries — so
// failures here never break pack.
func buildRepoSummary(files []models.FileRecord, info *project.Info) output.RepoSummary {
	langs := make(map[string]int, 8)
	symbols := 0
	for _, f := range files {
		langs[string(f.Lang)]++
		symbols += len(f.Symbols)
	}
	s := output.RepoSummary{
		Files:     len(files),
		Symbols:   symbols,
		Languages: langs,
	}
	if info != nil {
		s.Name = info.Label()
		if entries := info.EntryPoints(); len(entries) > 0 {
			s.Entry = filepath.ToSlash(entries[0])
		}
	}
	return s
}

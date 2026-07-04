package cli

import (
	"fmt"
	"os"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/embeddings"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/output"
	"github.com/neuromfs/neuromfs/internal/packager"
	"github.com/neuromfs/neuromfs/internal/project"
	"github.com/neuromfs/neuromfs/internal/ranking"
	"github.com/neuromfs/neuromfs/internal/retrieval"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/neuromfs/neuromfs/internal/taskflow"
	"github.com/spf13/cobra"
)

func newPackCmd() *cobra.Command {
	var (
		budget          int
		repoPath        string
		format          string
		outPath         string
		forTarget       string
		focus           string
		changed         bool
		maxFiles        int
		maxFragments    int
		saveBundle      string
		stripComments   bool
		stripBlankLines bool
		noChunks        bool
		machine         bool
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
			if err := validateBudget(budget); err != nil {
				return fmt.Errorf("pack: %w", err)
			}

			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("pack: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("pack: config: %w", err)
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

			embClient := embeddings.NewClient(cfg.HybridMode)
			queryEmb, _ := embClient.GetEmbedding(cmd.Context(), query)
			fileEmbs, _ := db.AllEmbeddings()

			rels, _ := db.AllRelations()
			info := loadProjectInfo(db)
			rankWeights, _, _ := ranking.LoadWeights(cfg.RepoRoot)
			rankOpts := ranking.Options{
				Project:        info,
				Focus:          focus,
				QueryEmbedding: queryEmb,
				Embeddings:     fileEmbs,
				Relations:      rels,
				Weights:        &rankWeights,
			}
			if changed {
				rankOpts.ChangedFiles = gitChangedFiles(cfg.RepoRoot)
				fmt.Fprintf(os.Stderr, "pack: --changed matched %d files from git\n", len(rankOpts.ChangedFiles))
			}

			// --for claude implies aggressive compression unless overridden:
			// prompts are cheaper when signatures replace full bodies.
			preferSig := output.Format(forTarget) == output.FormatClaude

			packOpts := packager.Options{
				Budget:           budget,
				MaxFiles:         maxFiles,
				MaxFragments:     maxFragments,
				PreferSignatures: preferSig,
				// Slack-fill is always desirable: without it, --for claude
				// regularly produced bundles that used <30 % of the budget
				// (top files clipped to signatures, no second pass to
				// promote them back). See packager.Pack second-pass loop.
				UpgradeWithSlack: true,
				// Query terms unlock sub-file excerpts on the top-ranked
				// TS/JS/Python files (see packager/excerpt.go).
				QueryTerms:      ranking.Tokenise(query),
				StripComments:   stripComments,
				StripBlankLines: stripBlankLines,
			}

			var bundle models.Bundle
			if !noChunks {
				searchLimit := maxFragments
				if searchLimit <= 0 {
					searchLimit = maxFiles
				}
				if searchLimit <= 0 {
					searchLimit = 12
				}
				searchRes, err := retrieval.Search(cmd.Context(), retrieval.Options{
					Query: query,
					Repo:  cfg.RepoRoot,
					Limit: searchLimit,
				})
				if err != nil {
					return fmt.Errorf("pack: chunk search: %w", err)
				}
				bundle, err = packager.PackChunks(chunkHitsFromSearch(searchRes, files), query, packOpts)
			} else {
				ranked := ranking.RankWithOptions(files, query, rankOpts)
				bundle, err = packager.Pack(ranked, query, packOpts)
			}
			if err != nil {
				return fmt.Errorf("pack: %w", err)
			}

			dest, err := os.Create(outPath)
			if err != nil {
				return fmt.Errorf("pack: create output file: %w", err)
			}
			defer dest.Close()

			if err := writeBundle(cfg.RepoRoot, dest, bundle, format, forTarget, files, info, machine); err != nil {
				return fmt.Errorf("pack: write: %w", err)
			}

			if saveBundle != "" {
				enriched := taskflow.EnrichBundle(bundle, cfg.RepoRoot)
				if err := taskflow.WriteBundleJSON(saveBundle, enriched); err != nil {
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
	cmd.Flags().BoolVar(&stripComments, "strip-comments", false, "Strip all comments from source files to optimize token budget")
	cmd.Flags().BoolVar(&stripBlankLines, "strip-blank-lines", false, "Strip all blank lines from source files to optimize token budget")
	cmd.Flags().BoolVar(&noChunks, "no-chunks", false, "Build the bundle from ranked whole files instead of code chunks")
	cmd.Flags().BoolVar(&machine, "machine", false, "Omit human explanations and scaffolding to save context tokens")
	_ = cmd.MarkFlagRequired("out")

	return cmd
}

func chunkHitsFromSearch(searchRes retrieval.Response, files []models.FileRecord) []packager.ChunkHit {
	langByPath := make(map[string]models.Lang, len(files))
	for _, file := range files {
		langByPath[file.RelPath] = file.Lang
	}
	hits := make([]packager.ChunkHit, 0, len(searchRes.Results))
	for _, hit := range searchRes.Results {
		hits = append(hits, packager.ChunkHit{
			RelPath:       hit.Path,
			Lang:          langByPath[hit.Path],
			StartLine:     hit.StartLine,
			EndLine:       hit.EndLine,
			Kind:          hit.Kind,
			Symbol:        hit.Symbol,
			Score:         hit.Score,
			Reasons:       hit.Reasons,
			TokenEstimate: hit.TokenEstimate,
			ContentHash:   hit.ContentHash,
			Snippet:       hit.Snippet,
		})
	}
	return hits
}

// writeBundle dispatches to the correct serialiser. The Claude path takes a
// RepoSummary derived from the already-loaded index, so the prompt gets
// repo orientation for free without another DB query.
func writeBundle(repoRoot string, dest *os.File, b models.Bundle, format, forTarget string, files []models.FileRecord, info *project.Info, machine bool) error {
	eff := effectiveFormat(format, forTarget)
	if eff == string(output.FormatClaude) {
		return output.WriteClaudeWithOptions(dest, b, taskflow.BuildRepoSummary(repoRoot, files, info), output.Options{Machine: machine})
	}
	return output.WriteWithOptions(dest, b, output.Format(eff), output.Options{Machine: machine})
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

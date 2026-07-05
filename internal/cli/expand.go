package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/contextladder"
	"github.com/neuromfs/neuromfs/internal/contextmap"
	"github.com/neuromfs/neuromfs/internal/contextusage"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/neuromfs/neuromfs/internal/taskflow"
	"github.com/spf13/cobra"
)

func newExpandCmd() *cobra.Command {
	var (
		repoPath string
		mode     string
		hash     string
		jsonOut  bool
		session  string
	)

	cmd := &cobra.Command{
		Use:   "expand <path[:start-end]|chunk_hash>",
		Short: "Expand an indexed file one step up the context ladder",
		Long: `Expand gives coding agents a controlled context ladder:
outline by default, line-ranged excerpt when a range/hash is provided, or
full file when explicitly requested.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("expand: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("expand: config: %w", err)
			}
			if err := taskflow.EnsureFreshIndex(cfg); err != nil {
				return fmt.Errorf("expand: auto-scan: %w", err)
			}

			db, err := storage.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("expand: open index: %w", err)
			}
			defer db.Close()

			files, err := db.AllFiles()
			if err != nil {
				return fmt.Errorf("expand: load files: %w", err)
			}
			rels, _ := db.AllRelations()

			spec := contextladder.ParseSpec(args[0])
			if hash != "" {
				spec.Hash = hash
			}
			rec, spec, err := contextladder.ResolveSpec(db, files, spec)
			if err != nil {
				return fmt.Errorf("expand: %w", err)
			}
			chunks, err := db.GetChunksForFile(rec.Path)
			if err != nil {
				return fmt.Errorf("expand: load chunks: %w", err)
			}
			contentBytes, err := os.ReadFile(rec.Path)
			if err != nil {
				return fmt.Errorf("expand: read %s: %w", rec.RelPath, err)
			}
			content := string(contentBytes)

			effMode, err := contextladder.EffectiveMode(mode, spec)
			if err != nil {
				return fmt.Errorf("expand: %w", err)
			}

			var loggedTokens int
			switch effMode {
			case contextladder.ModeOutline:
				logic := contextmap.Build(rec, files, chunks, rels, content)
				loggedTokens = contextladder.EstimateOutlineTokens(logic)
				if jsonOut {
					if err := json.NewEncoder(cmd.OutOrStdout()).Encode(logic); err != nil {
						return err
					}
				} else {
					contextladder.WriteOutline(cmd.OutOrStdout(), logic)
				}
			case contextladder.ModeExcerpt:
				out, err := contextladder.BuildExcerpt(rec, chunks, content, spec)
				if err != nil {
					return fmt.Errorf("expand: %w", err)
				}
				loggedTokens = out.Tokens
				if jsonOut {
					if err := json.NewEncoder(cmd.OutOrStdout()).Encode(out); err != nil {
						return err
					}
				} else {
					contextladder.WriteExpandedContent(cmd.OutOrStdout(), out)
				}
			case contextladder.ModeFull:
				out, err := contextladder.BuildFull(rec, content, spec)
				if err != nil {
					return fmt.Errorf("expand: %w", err)
				}
				loggedTokens = out.Tokens
				if jsonOut {
					if err := json.NewEncoder(cmd.OutOrStdout()).Encode(out); err != nil {
						return err
					}
				} else {
					contextladder.WriteExpandedContent(cmd.OutOrStdout(), out)
				}
			}
			if session != "" {
				_ = contextusage.Append(cfg.RepoRoot, contextusage.Entry{
					SessionID: session,
					Phase:     "expansion",
					Command:   "expand",
					Path:      rec.RelPath,
					Mode:      string(effMode),
					StartLine: spec.StartLine,
					EndLine:   spec.EndLine,
					Hash:      spec.Hash,
					Tokens:    loggedTokens,
					Bytes:     len(contentBytes),
				})
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().StringVar(&mode, "mode", string(contextladder.ModeAuto), "Expansion mode: auto | outline | excerpt | full")
	cmd.Flags().StringVar(&hash, "hash", "", "Require a matching file or chunk content hash")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "Print machine-readable JSON")
	cmd.Flags().StringVar(&session, "session", "", "Context usage session id for measurement")

	return cmd
}

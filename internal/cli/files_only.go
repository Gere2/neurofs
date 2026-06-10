package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/embeddings"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/ranking"
	"github.com/neuromfs/neuromfs/internal/storage"
)

type filesOnlyOptions struct {
	Limit        int
	MinScore     float64
	JSON         bool
	NoEmbeddings bool
}

type filesOnlyEntry struct {
	Path     string                   `json:"path"`
	Score    float64                  `json:"score"`
	Lang     models.Lang              `json:"lang"`
	Lines    int                      `json:"lines"`
	Size     int64                    `json:"size_bytes"`
	Checksum string                   `json:"checksum,omitempty"`
	Symbols  []models.Symbol          `json:"symbols,omitempty"`
	Imports  []string                 `json:"imports,omitempty"`
	Reasons  []models.InclusionReason `json:"reasons,omitempty"`
}

func validateFilesOnlyOptions(opts filesOnlyOptions) error {
	if opts.Limit < 0 {
		return fmt.Errorf("--limit must be >= 0")
	}
	if opts.MinScore < 0 {
		return fmt.Errorf("--min-score must be >= 0")
	}
	return nil
}

func rankFilesForCLI(ctx context.Context, cfg *config.Config, db *storage.DB, query string, files []models.FileRecord, noEmbeddings bool) []models.ScoredFile {
	var queryEmb []float32
	var fileEmbs map[string][]float32
	if !noEmbeddings {
		embClient := embeddings.NewClient(cfg.HybridMode)
		queryEmb, _ = embClient.GetEmbedding(ctx, query)
		fileEmbs, _ = db.AllEmbeddings()
	}
	rels, _ := db.AllRelations()
	return ranking.RankWithOptions(files, query, ranking.Options{
		Project:        loadProjectInfo(db),
		QueryEmbedding: queryEmb,
		Embeddings:     fileEmbs,
		Relations:      rels,
	})
}

func writeFilesOnly(w io.Writer, ranked []models.ScoredFile, opts filesOnlyOptions) error {
	if err := validateFilesOnlyOptions(opts); err != nil {
		return err
	}
	entries := filesOnlyEntries(ranked, opts)
	if opts.JSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}
	for _, entry := range entries {
		fmt.Fprintf(w, "%s (score=%.2f) - Reasons: %s\n",
			entry.Path, entry.Score, formatReasonsSingleLine(entry.Reasons))
	}
	return nil
}

func filesOnlyEntries(ranked []models.ScoredFile, opts filesOnlyOptions) []filesOnlyEntry {
	entries := make([]filesOnlyEntry, 0)
	for _, sf := range ranked {
		if sf.Score <= 0 || sf.Score < opts.MinScore {
			break
		}
		entries = append(entries, filesOnlyEntry{
			Path:     sf.Record.RelPath,
			Score:    sf.Score,
			Lang:     sf.Record.Lang,
			Lines:    sf.Record.Lines,
			Size:     sf.Record.Size,
			Checksum: sf.Record.Checksum,
			Symbols:  sf.Record.Symbols,
			Imports:  sf.Record.Imports,
			Reasons:  sf.Reasons,
		})
		if opts.Limit > 0 && len(entries) >= opts.Limit {
			break
		}
	}
	return entries
}

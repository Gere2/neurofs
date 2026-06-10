package packager

import (
	"fmt"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/tokenbudget"
)

// ChunkHit is the packager-facing projection of a ranked code chunk.
type ChunkHit struct {
	RelPath       string
	Lang          models.Lang
	StartLine     int
	EndLine       int
	Kind          string
	Symbol        string
	Score         float64
	Reasons       []string
	TokenEstimate int
	ContentHash   string
	Snippet       string
}

// PackChunks assembles a context bundle directly from ranked code chunks.
// It is intentionally parallel to Pack: same budget manager, same structural
// caps, same Bundle shape. The difference is that each fragment is already a
// sub-file excerpt, so we preserve line ranges and skip file-level signature
// fallbacks.
func PackChunks(hits []ChunkHit, query string, opts Options) (models.Bundle, error) {
	budget := tokenbudget.NewManager(opts.Budget)
	budget.Consume(headerReserve)

	var fragments []models.ContextFragment
	totalRawTokens := 0
	seenFiles := make(map[string]bool)

	for _, hit := range hits {
		if hit.Score < minScore {
			break
		}
		if budget.Remaining() <= 0 {
			break
		}
		if opts.MaxFragments > 0 && len(fragments) >= opts.MaxFragments {
			break
		}
		if opts.MaxFiles > 0 && !seenFiles[hit.RelPath] && len(seenFiles) >= opts.MaxFiles {
			continue
		}

		content := CompressCode(hit.Lang, hit.Snippet, opts.StripComments, opts.StripBlankLines)
		if strings.TrimSpace(content) == "" {
			continue
		}
		rawTokens := hit.TokenEstimate
		if rawTokens <= 0 {
			rawTokens = tokenbudget.EstimateTokens(content)
		}
		totalRawTokens += rawTokens

		excerpt := formatChunkExcerpt(hit, content)
		tokens := tokenbudget.EstimateTokens(excerpt)
		if tokens <= 0 || !budget.CanFit(tokens) {
			continue
		}

		budget.Consume(tokens)
		seenFiles[hit.RelPath] = true
		fragments = append(fragments, models.ContextFragment{
			RelPath:        hit.RelPath,
			Lang:           hit.Lang,
			Representation: models.RepExcerpt,
			Content:        excerpt,
			Tokens:         tokens,
			Score:          hit.Score,
			Reasons:        chunkReasons(hit),
			StartLine:      hit.StartLine,
			EndLine:        hit.EndLine,
			ContentHash:    hit.ContentHash,
		})
	}

	var compressionRatio float64
	netUsed := budget.Used() - headerReserve
	if netUsed > 0 && totalRawTokens > 0 {
		compressionRatio = float64(totalRawTokens) / float64(netUsed)
	}

	return models.Bundle{
		Query:     query,
		Budget:    opts.Budget,
		Fragments: fragments,
		Stats: models.BundleStats{
			FilesConsidered:  len(hits),
			FilesIncluded:    len(fragments),
			TokensUsed:       budget.Used(),
			TokensBudget:     opts.Budget,
			CompressionRatio: compressionRatio,
		},
	}, nil
}

func formatChunkExcerpt(hit ChunkHit, content string) string {
	label := strings.TrimSpace(hit.Symbol)
	if label == "" {
		label = strings.TrimSpace(hit.Kind)
	}
	if label == "" {
		label = "chunk"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "// file: %s\n", hit.RelPath)
	fmt.Fprintf(&sb, "// lines: %d-%d\n", hit.StartLine, hit.EndLine)
	fmt.Fprintf(&sb, "// chunk: %s\n", label)
	if hit.ContentHash != "" {
		fmt.Fprintf(&sb, "// content_hash: %s\n", hit.ContentHash)
	}
	sb.WriteString("\n")
	sb.WriteString(strings.TrimRight(content, "\n"))
	return sb.String()
}

func chunkReasons(hit ChunkHit) []models.InclusionReason {
	if len(hit.Reasons) == 0 {
		return []models.InclusionReason{{
			Signal: "chunk_search",
			Detail: fmt.Sprintf("%s:%d-%d", hit.RelPath, hit.StartLine, hit.EndLine),
			Weight: hit.Score,
		}}
	}
	reasons := make([]models.InclusionReason, 0, len(hit.Reasons))
	for _, reason := range hit.Reasons {
		reason = strings.TrimSpace(reason)
		if reason == "" {
			continue
		}
		reasons = append(reasons, models.InclusionReason{
			Signal: reason,
			Detail: fmt.Sprintf("%s:%d-%d", hit.RelPath, hit.StartLine, hit.EndLine),
			Weight: 1.0,
		})
	}
	if len(reasons) == 0 {
		return chunkReasons(ChunkHit{
			RelPath:   hit.RelPath,
			StartLine: hit.StartLine,
			EndLine:   hit.EndLine,
			Score:     hit.Score,
		})
	}
	return reasons
}

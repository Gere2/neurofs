// Package output renders a Bundle in the requested format.
package output

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
)

// Format is an output serialisation format.
type Format string

const (
	FormatMarkdown Format = "markdown"
	FormatJSON     Format = "json"
	FormatText     Format = "text"
	// FormatClaude is an opinionated, prompt-shaped output: task first,
	// then the context blocks, then explicit grounding instructions. It is
	// meant for pasting into a fresh Claude conversation.
	FormatClaude Format = "claude"
)

// Write serialises bundle to w in the given format. For FormatClaude with
// no repo summary available, pass a zero RepoSummary to WriteClaude
// directly; this dispatcher uses an empty one.
func Write(w io.Writer, bundle models.Bundle, format Format) error {
	switch format {
	case FormatJSON:
		return writeJSON(w, bundle)
	case FormatText:
		return writeText(w, bundle)
	case FormatClaude:
		return WriteClaude(w, bundle, RepoSummary{})
	default:
		return writeMarkdown(w, bundle)
	}
}

// ─── Markdown ────────────────────────────────────────────────────────────────

func writeMarkdown(w io.Writer, b models.Bundle) error {
	p := func(format string, args ...any) {
		fmt.Fprintf(w, format, args...)
	}

	p("# NeuroFS Context Bundle\n\n")
	p("> **Query:** %s\n>\n", b.Query)
	p("> **Budget:** %d tokens | **Used:** %d tokens (%.1f%%) | **Files:** %d/%d",
		b.Stats.TokensBudget,
		b.Stats.TokensUsed,
		pct(b.Stats.TokensUsed, b.Stats.TokensBudget),
		b.Stats.FilesIncluded,
		b.Stats.FilesConsidered,
	)
	if b.Stats.CompressionRatio > 0 {
		p(" | **Compression:** %.1fx", b.Stats.CompressionRatio)
	}
	p("\n\n---\n\n")

	for i, frag := range b.Fragments {
		p("## [%d] `%s`\n\n", i+1, frag.RelPath)

		p("**Representation:** `%s` | **Score:** %.2f | **Tokens:** %d\n\n",
			frag.Representation, frag.Score, frag.Tokens)

		if len(frag.Reasons) > 0 {
			p("**Included because:**\n")
			// De-duplicate reasons by signal+detail before printing.
			seen := make(map[string]bool)
			for _, r := range frag.Reasons {
				key := r.Signal + ":" + r.Detail
				if seen[key] {
					continue
				}
				seen[key] = true
				p("- `%s`: %s (%.1f)\n", r.Signal, r.Detail, r.Weight)
			}
			p("\n")
		}

		lang := langFence(frag.Lang)
		p("```%s\n%s\n```\n\n", lang, strings.TrimRight(frag.Content, "\n"))

		if i < len(b.Fragments)-1 {
			p("---\n\n")
		}
	}

	if len(b.Fragments) == 0 {
		p("_No relevant context found for this query within the token budget._\n")
	}

	return nil
}

// ─── JSON ────────────────────────────────────────────────────────────────────

func writeJSON(w io.Writer, b models.Bundle) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(b)
}

// ─── Plain text ──────────────────────────────────────────────────────────────

func writeText(w io.Writer, b models.Bundle) error {
	p := func(format string, args ...any) {
		fmt.Fprintf(w, format, args...)
	}

	p("NEUROFS CONTEXT BUNDLE\n")
	p("======================\n")
	p("Query  : %s\n", b.Query)
	p("Budget : %d tokens\n", b.Stats.TokensBudget)
	p("Used   : %d tokens (%.1f%%)\n",
		b.Stats.TokensUsed, pct(b.Stats.TokensUsed, b.Stats.TokensBudget))
	p("Files  : %d included / %d considered\n",
		b.Stats.FilesIncluded, b.Stats.FilesConsidered)
	if b.Stats.CompressionRatio > 0 {
		p("Ratio  : %.1fx compression\n", b.Stats.CompressionRatio)
	}
	p("\n")

	for i, frag := range b.Fragments {
		p("─────────────────────────────────────\n")
		p("[%d] %s\n", i+1, frag.RelPath)
		p("    representation : %s\n", frag.Representation)
		p("    score          : %.2f\n", frag.Score)
		p("    tokens         : %d\n", frag.Tokens)

		seen := make(map[string]bool)
		for _, r := range frag.Reasons {
			key := r.Signal + ":" + r.Detail
			if seen[key] {
				continue
			}
			seen[key] = true
			p("    reason         : [%s] %s\n", r.Signal, r.Detail)
		}
		p("\n%s\n\n", frag.Content)
	}

	if len(b.Fragments) == 0 {
		p("No relevant context found.\n")
	}

	return nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

func pct(used, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

func langFence(lang models.Lang) string {
	switch lang {
	case models.LangTypeScript:
		return "typescript"
	case models.LangJavaScript:
		return "javascript"
	case models.LangPython:
		return "python"
	case models.LangGo:
		return "go"
	case models.LangMarkdown:
		return "markdown"
	case models.LangJSON:
		return "json"
	case models.LangYAML:
		return "yaml"
	default:
		return ""
	}
}

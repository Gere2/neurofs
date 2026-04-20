package output

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
)

// RepoSummary is a short, token-cheap profile of the repository the bundle
// was pulled from. Callers that have this context (language breakdown,
// project name) pass it in; the Claude writer inlines it so the model gets
// repo orientation without a separate tool call.
//
// Any field may be empty — the writer prints only what is set.
type RepoSummary struct {
	Name      string         // display name, e.g. from package.json
	Languages map[string]int // lang → file count
	Files     int            // total indexed files
	Symbols   int            // total indexed symbols
	Entry     string         // best-effort primary entry point path
}

// WriteClaude renders a prompt-shaped view of the bundle, ready to paste
// into a fresh Claude conversation. Compared to the markdown writer this
// one:
//
//  1. Leads with the task, not the metadata, so the model reads the goal
//     before the evidence.
//  2. Drops heavy formatting (emojis, tables, horizontal rules) that buys
//     little and costs tokens.
//  3. Ends with explicit guardrails: cite paths, refuse to invent beyond
//     the bundle, ask for more context when needed.
//
// The summary argument is optional; pass a zero RepoSummary to skip the
// repo-orientation block entirely.
func WriteClaude(w io.Writer, b models.Bundle, summary RepoSummary) error {
	p := func(format string, args ...any) {
		fmt.Fprintf(w, format, args...)
	}

	p("<task>\n%s\n</task>\n\n", strings.TrimSpace(b.Query))

	if hasSummary(summary) {
		p("<repo>\n")
		if summary.Name != "" {
			p("name: %s\n", summary.Name)
		}
		if summary.Files > 0 {
			p("files indexed: %d\n", summary.Files)
		}
		if summary.Symbols > 0 {
			p("symbols indexed: %d\n", summary.Symbols)
		}
		if summary.Entry != "" {
			p("entry point: %s\n", summary.Entry)
		}
		if len(summary.Languages) > 0 {
			p("languages: %s\n", formatLanguages(summary.Languages))
		}
		p("</repo>\n\n")
	}

	p("<selection>\n")
	p("bundle: %d files included of %d considered, %d tokens used of %d budgeted",
		b.Stats.FilesIncluded, b.Stats.FilesConsidered,
		b.Stats.TokensUsed, b.Stats.TokensBudget)
	if b.Stats.CompressionRatio > 0 {
		p(" (%.1fx compression)", b.Stats.CompressionRatio)
	}
	p("\n</selection>\n\n")

	p("<context>\n")
	for i, frag := range b.Fragments {
		reasons := summariseReasons(frag.Reasons)
		p("<file path=%q rep=%q tokens=%d%s>\n",
			filepath.ToSlash(frag.RelPath),
			string(frag.Representation),
			frag.Tokens,
			reasonsAttr(reasons),
		)
		p("%s\n", strings.TrimRight(frag.Content, "\n"))
		p("</file>\n")
		if i < len(b.Fragments)-1 {
			p("\n")
		}
	}
	p("</context>\n\n")

	p("<instructions>\n")
	p("- Treat the files above as the only source of truth.\n")
	p("- When you make a claim about the code, cite it as `path:line`.\n")
	p("- If a fragment is shown as a `signature` or `structural_note`, do not assume the body — ask for it.\n")
	p("- If anything you need is missing, say which path you want expanded instead of guessing.\n")
	p("</instructions>\n")

	if len(b.Fragments) == 0 {
		p("\n(no context available — the bundle is empty)\n")
	}
	return nil
}

// hasSummary reports whether a RepoSummary carries any printable data.
func hasSummary(s RepoSummary) bool {
	return s.Name != "" || s.Files > 0 || s.Symbols > 0 || s.Entry != "" || len(s.Languages) > 0
}

// summariseReasons collapses the per-fragment reasons to at most three
// signal names, sorted by aggregate weight. That gives the model a hint
// about why a fragment was picked without pasting the full scoring table.
func summariseReasons(reasons []models.InclusionReason) []string {
	if len(reasons) == 0 {
		return nil
	}
	weights := make(map[string]float64, len(reasons))
	for _, r := range reasons {
		weights[r.Signal] += r.Weight
	}
	names := make([]string, 0, len(weights))
	for n := range weights {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return weights[names[i]] > weights[names[j]] })
	if len(names) > 3 {
		names = names[:3]
	}
	return names
}

// reasonsAttr renders a short ` reasons="..."` XML attribute, or "" when no
// reasons exist. Caller is responsible for the leading space inside the
// tag template.
func reasonsAttr(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	return fmt.Sprintf(` reasons=%q`, strings.Join(reasons, ","))
}

// formatLanguages emits a token-cheap `lang:count, lang:count` list sorted
// by descending count so the dominant languages appear first.
func formatLanguages(langs map[string]int) string {
	type kv struct {
		k string
		v int
	}
	pairs := make([]kv, 0, len(langs))
	for k, v := range langs {
		pairs = append(pairs, kv{k, v})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].v != pairs[j].v {
			return pairs[i].v > pairs[j].v
		}
		return pairs[i].k < pairs[j].k
	})
	parts := make([]string, 0, len(pairs))
	for _, p := range pairs {
		parts = append(parts, fmt.Sprintf("%s:%d", p.k, p.v))
	}
	return strings.Join(parts, ", ")
}

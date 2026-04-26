// Package packager assembles a context bundle from ranked files,
// respecting the token budget and selecting the right representation
// for each fragment.
package packager

import (
	"fmt"
	"os"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/parser"
	"github.com/neuromfs/neuromfs/internal/tokenbudget"
)

// Thresholds for representation selection (measured in tokens).
const (
	// fullCodeMaxTokens: files smaller than this are candidates for full_code.
	fullCodeMaxTokens = 600

	// aggressiveFullCodeMaxTokens is the threshold used when the caller asks
	// for aggressive compression — signatures are preferred unless the file
	// is genuinely tiny.
	aggressiveFullCodeMaxTokens = 180

	// Minimum score required for a file to enter the bundle at all.
	minScore = 0.1

	// Reserve this many tokens for the bundle header.
	headerReserve = 80
)

// Options configures bundle assembly.
//
// MaxFiles / MaxFragments are structural caps: they stop packing before the
// token budget is reached when the caller wants a predictably small bundle
// regardless of budget slack. Zero means "no cap".
//
// PreferSignatures trades fidelity for tokens: small files still go full_code,
// but anything larger than aggressiveFullCodeMaxTokens collapses to a
// signature or a structural note even when the budget could fit it verbatim.
//
// UpgradeWithSlack enables a second pass that, after the initial pack, walks
// fragments in score order and promotes signatures to full_code whenever the
// remaining budget can absorb the delta. Without it, a Strategy-shaped pack
// can leave 80%+ of the budget unused while every top file shows up as a
// 30-token signature.
type Options struct {
	Budget           int
	MaxFiles         int
	MaxFragments     int
	PreferSignatures bool
	UpgradeWithSlack bool
}

// upgradeCandidate holds the per-fragment data we need for the optional
// second-pass body upgrade. We keep it package-private; callers never see it.
type upgradeCandidate struct {
	idx      int    // index into the fragments slice
	path     string // absolute path on disk
	rawToks  int    // tokens needed to upgrade to full_code
}

// Pack takes a ranked list of scored files and assembles an auditable Bundle.
func Pack(ranked []models.ScoredFile, query string, opts Options) (models.Bundle, error) {
	budget := tokenbudget.NewManager(opts.Budget)
	budget.Consume(headerReserve)

	var fragments []models.ContextFragment
	var upgrades []upgradeCandidate
	totalRawTokens := 0

	for _, sf := range ranked {
		if sf.Score < minScore {
			break // list is sorted; stop at the first irrelevant file
		}
		if budget.Remaining() <= 0 {
			break
		}
		if opts.MaxFiles > 0 && len(fragments) >= opts.MaxFiles {
			break
		}
		if opts.MaxFragments > 0 && len(fragments) >= opts.MaxFragments {
			break
		}

		content, err := os.ReadFile(sf.Record.Path)
		if err != nil {
			continue
		}

		rawTokens := tokenbudget.EstimateTokens(string(content))
		totalRawTokens += rawTokens

		frag := selectFragment(sf, string(content), rawTokens, budget, opts)
		if frag == nil {
			continue // nothing fits even as a structural note
		}

		budget.Consume(frag.Tokens)
		fragments = append(fragments, *frag)

		if opts.UpgradeWithSlack && frag.Representation == models.RepSignature {
			upgrades = append(upgrades, upgradeCandidate{
				idx:     len(fragments) - 1,
				path:    sf.Record.Path,
				rawToks: rawTokens,
			})
		}
	}

	// Second pass: promote signatures to full_code while slack remains.
	// Fragments are already in score order, so iterating upgrades in order
	// preserves the "best file gets the body first" property. We cap the
	// upgrade at fullCodeMaxTokens — past that, one monolithic body would
	// crowd out coverage from all the smaller signatures we still want.
	for _, u := range upgrades {
		if u.rawToks > fullCodeMaxTokens {
			continue
		}
		current := fragments[u.idx]
		delta := u.rawToks - current.Tokens
		if delta <= 0 || !budget.CanFit(delta) {
			continue
		}
		content, err := os.ReadFile(u.path)
		if err != nil {
			continue
		}
		fragments[u.idx].Representation = models.RepFullCode
		fragments[u.idx].Content = string(content)
		fragments[u.idx].Tokens = u.rawToks
		budget.Consume(delta)
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
			FilesConsidered:  len(ranked),
			FilesIncluded:    len(fragments),
			TokensUsed:       budget.Used(),
			TokensBudget:     opts.Budget,
			CompressionRatio: compressionRatio,
		},
	}, nil
}

// selectFragment decides the best representation for a scored file given the
// remaining budget, trying from most informative to least.
func selectFragment(sf models.ScoredFile, content string, rawTokens int, budget *tokenbudget.Manager, opts Options) *models.ContextFragment {
	base := &models.ContextFragment{
		RelPath: sf.Record.RelPath,
		Lang:    sf.Record.Lang,
		Score:   sf.Score,
		Reasons: sf.Reasons,
	}

	fullCap := fullCodeMaxTokens
	if opts.PreferSignatures {
		fullCap = aggressiveFullCodeMaxTokens
	}

	// Option 1: full file — only for small files where budget allows.
	if rawTokens <= fullCap && budget.CanFit(rawTokens) {
		f := *base
		f.Representation = models.RepFullCode
		f.Content = content
		f.Tokens = rawTokens
		return &f
	}

	// Option 2: signature — compact interface view.
	sig := buildSignature(sf, content)
	sigTokens := tokenbudget.EstimateTokens(sig)
	if sigTokens > 0 && budget.CanFit(sigTokens) {
		f := *base
		f.Representation = models.RepSignature
		f.Content = sig
		f.Tokens = sigTokens
		return &f
	}

	// Option 3: structural note — absolute minimum, just metadata.
	note := buildStructuralNote(sf)
	noteTokens := tokenbudget.EstimateTokens(note)
	if noteTokens > 0 && budget.CanFit(noteTokens) {
		f := *base
		f.Representation = models.RepStructuralNote
		f.Content = note
		f.Tokens = noteTokens
		return &f
	}

	return nil // budget exhausted, nothing fits
}

// buildSignature returns a compact signature derived from the parser output.
func buildSignature(sf models.ScoredFile, content string) string {
	parsed := parser.Parse(sf.Record.Lang, content)
	if parsed.Signature == "" {
		return buildStructuralNote(sf)
	}
	return fmt.Sprintf(
		"// file: %s\n// lang: %s\n// representation: signature\n\n%s",
		sf.Record.RelPath, sf.Record.Lang, parsed.Signature,
	)
}

// buildStructuralNote returns a minimal metadata description of the file.
func buildStructuralNote(sf models.ScoredFile) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "// file: %s\n", sf.Record.RelPath)
	fmt.Fprintf(&sb, "// lang: %s\n", sf.Record.Lang)
	fmt.Fprintf(&sb, "// representation: structural_note\n")
	fmt.Fprintf(&sb, "// size: %d lines, %d bytes\n", sf.Record.Lines, sf.Record.Size)

	if len(sf.Record.Symbols) > 0 {
		names := make([]string, 0, len(sf.Record.Symbols))
		for _, s := range sf.Record.Symbols {
			names = append(names, s.Name)
		}
		fmt.Fprintf(&sb, "// symbols: %s\n", strings.Join(names, ", "))
	}

	if len(sf.Record.Imports) > 0 {
		fmt.Fprintf(&sb, "// imports: %s\n", strings.Join(sf.Record.Imports, ", "))
	}

	return sb.String()
}

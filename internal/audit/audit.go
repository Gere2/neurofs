package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/neuromfs/neuromfs/internal/models"
)

// Options drive a single audit run. ExpectsFacts is optional — when unset,
// AnswerRecall stays at 0 and is not included in summaries.
type Options struct {
	ExpectsFacts []string
	// Now overrides the clock for deterministic tests. Leave nil in prod.
	Now func() time.Time
}

// Run takes a bundle, asks the model, parses and validates citations,
// detects drift, and returns a fully-populated AuditRecord. The record is
// self-contained: every field needed to replay or compare runs later is
// captured here.
//
// Errors only propagate from the model call; parsing never fails, drift
// never fails — empty results are normal signal.
func Run(ctx context.Context, m Model, bundle models.Bundle, opts Options) (AuditRecord, error) {
	now := time.Now
	if opts.Now != nil {
		now = opts.Now
	}

	prompt := BuildPrompt(bundle)
	resp, err := m.Generate(ctx, prompt)
	if err != nil {
		return AuditRecord{}, fmt.Errorf("audit: model.Generate: %w", err)
	}

	citations := ValidateCitations(ParseCitations(resp), bundle)
	drift := DetectDrift(resp, bundle)

	var (
		factsHit []string
		recall   float64
	)
	if len(opts.ExpectsFacts) > 0 {
		factsHit, recall = ScoreFacts(resp, opts.ExpectsFacts)
	}

	return AuditRecord{
		Question:      bundle.Query,
		Model:         m.ID(),
		Timestamp:     now(),
		BundleHash:    BundleHash(bundle),
		Fragments:     freezeFragments(bundle),
		Response:      resp,
		Citations:     citations,
		Drift:         drift,
		GroundedRatio: GroundedRatio(citations),
		ExpectsFacts:  opts.ExpectsFacts,
		FactsHit:      factsHit,
		AnswerRecall:  recall,
	}, nil
}

// BuildPrompt composes the model input from the bundle. This is the same
// text future CLI wiring would send; keeping it here means every audit
// replay uses the exact prompt that was evaluated.
func BuildPrompt(bundle models.Bundle) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Question: %s\n\n", bundle.Query)
	sb.WriteString("You have the following source fragments. Cite them as `path:line` when you rely on them. If something is not in the bundle, say so — do not invent.\n\n")
	for _, f := range bundle.Fragments {
		fmt.Fprintf(&sb, "=== %s (%s, %s) ===\n", f.RelPath, f.Lang, f.Representation)
		sb.WriteString(f.Content)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// BundleHash produces a stable sha256 over the bundle's query and
// fragments. Used as an identity for replay: two runs with the same hash
// received the exact same context.
func BundleHash(bundle models.Bundle) string {
	h := sha256.New()
	h.Write([]byte(bundle.Query))
	h.Write([]byte{0})

	paths := make([]string, 0, len(bundle.Fragments))
	by := make(map[string]models.ContextFragment, len(bundle.Fragments))
	for _, f := range bundle.Fragments {
		paths = append(paths, f.RelPath)
		by[f.RelPath] = f
	}
	sort.Strings(paths)

	for _, p := range paths {
		f := by[p]
		h.Write([]byte(f.RelPath))
		h.Write([]byte{0})
		h.Write([]byte(f.Representation))
		h.Write([]byte{0})
		h.Write([]byte(f.Content))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// freezeFragments copies the minimal fields needed for replay. We drop
// per-fragment scoring reasons — those live in the ranking layer and are
// already re-derivable from a scan.
func freezeFragments(bundle models.Bundle) []AuditFragment {
	if len(bundle.Fragments) == 0 {
		return nil
	}
	out := make([]AuditFragment, len(bundle.Fragments))
	for i, f := range bundle.Fragments {
		out[i] = AuditFragment{
			RelPath:        f.RelPath,
			Lang:           f.Lang,
			Representation: f.Representation,
			Tokens:         f.Tokens,
			Content:        f.Content,
		}
	}
	return out
}

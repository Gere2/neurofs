package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/runid"
)

// BundleHashAlgorithm identifies the canonical prompt serialization used by
// BundleHash. Persisting it prevents old, order-insensitive hashes from being
// mistaken for the current exact-prompt identity.
const BundleHashAlgorithm = "sha256:neurofs-audit-prompt-v2"

// GroundingMetricVersion identifies the citation validation semantics used by
// new records: visible line ranges and a zero score for absent citations.
const GroundingMetricVersion = 2

// Options drive a single audit run. ExpectsFacts is optional — when unset,
// AnswerRecall stays at 0 and is not included in summaries. Mode is an
// opaque label (strategy / build / review today) that the caller may set
// so the resulting AuditRecord carries the intent the bundle was packed
// for. Leave empty when the distinction does not apply.
type Options struct {
	ExpectsFacts []string
	Mode         string
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
	attribution, err := runid.Bind(ctx, runid.Availability{})
	if err != nil {
		return AuditRecord{}, fmt.Errorf("audit: bind run identity: %w", err)
	}
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

	rec := AuditRecord{
		Availability:  attribution,
		Question:      bundle.Query,
		Model:         m.ID(),
		Mode:          opts.Mode,
		Timestamp:     now(),
		BundleHash:    BundleHash(bundle),
		HashAlgorithm: BundleHashAlgorithm,
		Fragments:     freezeFragments(bundle),
		Response:      resp,
		Citations:     citations,
		Drift:         drift,
		GroundedRatio: GroundedRatio(citations),
		MetricVersion: GroundingMetricVersion,
		ExpectsFacts:  opts.ExpectsFacts,
		FactsHit:      factsHit,
		AnswerRecall:  recall,
	}
	// Freeze the cost ledger only when the packager actually produced
	// something. A zero BundleStats (e.g. empty fixture in tests, or a
	// bundle the caller assembled by hand) would otherwise persist as a
	// misleading "0 tokens, 0 files" record.
	if bundle.Stats.TokensUsed > 0 {
		stats := bundle.Stats
		rec.Stats = &stats
	}
	return rec, nil
}

// BuildPrompt composes the model input from the bundle. This is the same
// text future CLI wiring would send; keeping it here means every audit
// replay uses the exact prompt that was evaluated.
func BuildPrompt(bundle models.Bundle) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Question: %s\n\n", bundle.Query)
	sb.WriteString("You have the following source fragments. Cite them as `path:line` when you rely on them. If something is not in the bundle, say so — do not invent.\n\n")
	for _, f := range bundle.Fragments {
		location := f.RelPath
		if f.StartLine > 0 && f.EndLine >= f.StartLine {
			location = fmt.Sprintf("%s:%d-%d", location, f.StartLine, f.EndLine)
		}
		fmt.Fprintf(&sb, "=== %s (%s, %s) ===\n", location, f.Lang, f.Representation)
		sb.WriteString(f.Content)
		sb.WriteString("\n\n")
	}
	return sb.String()
}

// BundleHash is the sha256 of the exact prompt bytes sent to the model. Order,
// duplicate paths, language, representation and visible line ranges all affect
// BuildPrompt and therefore the identity.
func BundleHash(bundle models.Bundle) string {
	sum := sha256.Sum256([]byte(BuildPrompt(bundle)))
	return hex.EncodeToString(sum[:])
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
			StartLine:      f.StartLine,
			EndLine:        f.EndLine,
			ContentHash:    f.ContentHash,
		}
	}
	return out
}

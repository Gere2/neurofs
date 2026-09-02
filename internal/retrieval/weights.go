package retrieval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Gere2/neurofs/internal/atomicfile"
	"github.com/Gere2/neurofs/internal/fsutil"
)

// Weights holds every tunable scoring weight used by chunk search. The
// zero value is NOT usable — always start from DefaultWeights (whose
// values are the hand-calibrated constants the engine shipped with) and
// override from there. Persisted per-repo at .neurofs/weights.json so
// `neurofs learn tune --apply` can improve ranking from accumulated
// usage/feedback signal without a rebuild.
type Weights struct {
	// Additive boosts applied while scoring a chunk against query terms.
	SymbolMatch             float64 `json:"symbol_match"`
	SymbolExact             float64 `json:"symbol_exact"`
	PathMatch               float64 `json:"path_match"`
	KindMatch               float64 `json:"kind_match"`
	ContentMatch            float64 `json:"content_match"` // per matching term, capped at 3 terms
	ChunkScope              float64 `json:"chunk_scope"`
	StructuralSymbol        float64 `json:"structural_symbol"` // exact structural symbol hit; also caps the per-file total
	StructuralSymbolPartial float64 `json:"structural_symbol_partial"`
	StructuralImport        float64 `json:"structural_import"` // per matching import, capped at 10.0 total
	Semantic                float64 `json:"semantic"`          // scale and ceiling of the semantic boost
	WorkingSet              float64 `json:"working_set"`
	ExactContent            float64 `json:"exact_content"`
	ExactFilename           float64 `json:"exact_filename"`
	Graph                   float64 `json:"graph"`
	// ImplKind boosts function-bodied chunks (func/method/nested closures)
	// over same-evidence declaration chunks. Motivated on vuejs/core, where
	// 5-token `export default Vue` stubs and short type aliases fill the
	// top-8 on symbol identity alone while the implementations the facts
	// live in never surface. Ships at 0 (inert): the tuner probes zero
	// weights with fixed candidate values, so only measured cross-corpus
	// evidence turns it on.
	ImplKind float64 `json:"impl_kind"`
	// HeadingPathMatch boosts a document section whose heading chain names a
	// query term exactly — query "S6.1" against "Roadmap/Sprint S6/S6.1
	// Cancelación". Section identifiers like that are invisible to BM25:
	// they tokenise away to nothing, so before this the ranker had no way to
	// prefer the requested subsection over its siblings.
	HeadingPathMatch float64 `json:"heading_path_match"`

	// Penalties.
	LongChunkPenaltyMax float64 `json:"long_chunk_penalty_max"`
	// TestDownrank, TinyChunkKeep and LegacyPathKeep are multiplicative
	// keep-fractions in (0, 1]: a penalised chunk keeps exactly that
	// fraction of its score, and 1.0 is neutral. Until 2026-09-02 all
	// three helpers in search.go applied the fraction twice — they scaled
	// the score and then handed addPenalty the difference, which
	// addPenalty subtracts again — so the effective keep was 2*keep-1
	// clamped at 0. Every measurement below that names a keep value was
	// taken under that behaviour; the annotations say what was actually
	// applied.
	TestDownrank  float64 `json:"test_downrank"`
	TinyChunkKeep float64 `json:"tiny_chunk_keep"` // sub-40-token chunks
	// LegacyPathKeep is the keep-fraction for chunks under compat/legacy
	// directories when the query does not ask for that surface. Neutral at
	// 1.0 and measured to stay there (2026-07-04): the 3-corpus tuner
	// declined 0.6/0.8, and a manual 0.3 probe left recall identical on
	// every shape while raising vue tokens 758 → 1154 — compat stubs were
	// no longer the binding constraint once impl_kind landed. Those probes
	// ran double-applied, so they measured effective keeps of 0.2/0.6 and
	// 0 (full suppression); "1.0 is where this belongs" survives, but no
	// mild legacy downrank has actually been tested. Kept for
	// re-exploration as fixtures grow.
	LegacyPathKeep float64 `json:"legacy_path_keep"`
	// PathAffinityKeep is the keep-fraction applied to chunks that live
	// outside the dominant top-1 directory and are not imported by the
	// top-1 file. It only engages when one result clearly dominates the
	// ranking (see pathAffinityDominanceRatio): a decisive top hit means
	// the question has a home directory, and results from unrelated trees
	// are lexical collisions rather than answers. Measured origin: on
	// raiz-app, a query answered by apps/brain/lib/inventory/consumption/
	// at score 110 also pulled apps/brain/lib/gmail/oauth.ts at 21 on the
	// word "identity" alone.
	PathAffinityKeep float64 `json:"path_affinity_keep"`
}

const maxWeightsFileSize int64 = 1 << 20

// DefaultWeights returns the hand-calibrated values the scoring constants
// held before weights became tunable. Any change here shifts ranking for
// every repo without a weights.json, so treat it like a ranking change.
func DefaultWeights() Weights {
	return Weights{
		SymbolMatch:             8.0,
		SymbolExact:             6.0,
		PathMatch:               3.0,
		KindMatch:               1.0,
		ContentMatch:            2.0,
		ChunkScope:              0.5,
		StructuralSymbol:        18.0,
		StructuralSymbolPartial: 3.0,
		StructuralImport:        2.0,
		Semantic:                8.0,
		WorkingSet:              2.25,
		ExactContent:            3.75,
		ExactFilename:           4.25,
		Graph:                   1.25,
		ImplKind:                0.0,
		HeadingPathMatch:        10.0,
		LongChunkPenaltyMax:     4.0,
		TestDownrank:            0.72,
		// Neutral by default: downranking tiny chunks was A/B-tested on
		// three corpora (2026-07-04) and lost — click recall 66.7% → 53.3%
		// at keep=0.7, tokens up on every shape (tiny stubs are cheap).
		// That probe was double-applied (effective keep 0.4), so what lost
		// was a harsher penalty than its label; a true 0.7 is untested.
		// The knob stays so the tuner can re-explore it as fixtures grow,
		// but only evidence should ever move it off 1.0.
		TinyChunkKeep:    1.0,
		LegacyPathKeep:   1.0,
		PathAffinityKeep: 0.6,
	}
}

// WeightsPath returns where tuned weights live for repoRoot.
func WeightsPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".neurofs", "weights.json")
}

// LoadWeights reads tuned weights for repoRoot, layered over defaults so a
// weights.json written by an older binary keeps sane values for fields it
// does not know about. The bool reports whether a weights file existed.
// On unreadable/malformed files it returns defaults plus the error; Search
// deliberately ignores that error (a broken optional file must not take
// retrieval down) while `neurofs learn status` surfaces it.
func LoadWeights(repoRoot string) (Weights, bool, error) {
	w := DefaultWeights()
	data, _, err := fsutil.ReadRegularFileBounded(WeightsPath(repoRoot), maxWeightsFileSize)
	if err != nil {
		if os.IsNotExist(err) {
			return w, false, nil
		}
		return w, false, fmt.Errorf("weights: read: %w", err)
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return DefaultWeights(), true, fmt.Errorf("weights: parse %s: %w", WeightsPath(repoRoot), err)
	}
	w.Clamp()
	return w, true, nil
}

// SaveWeights persists w for repoRoot, creating .neurofs on first use.
func SaveWeights(repoRoot string, w Weights) error {
	w.Clamp()
	p := WeightsPath(repoRoot)
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return fmt.Errorf("weights: marshal: %w", err)
	}
	if err := atomicfile.WriteFile(p, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("weights: write: %w", err)
	}
	return nil
}

// Clamp bounds every weight to its safe range: additive boosts to [0, 60]
// and the test keep-fraction to (0, 1] — above 1.0 it would silently turn
// a penalty into a boost for test files.
func (w *Weights) Clamp() {
	clampBoost := func(v *float64) {
		if *v < 0 {
			*v = 0
		}
		if *v > 60 {
			*v = 60
		}
	}
	clampBoost(&w.SymbolMatch)
	clampBoost(&w.SymbolExact)
	clampBoost(&w.PathMatch)
	clampBoost(&w.KindMatch)
	clampBoost(&w.ContentMatch)
	clampBoost(&w.ChunkScope)
	clampBoost(&w.StructuralSymbol)
	clampBoost(&w.StructuralSymbolPartial)
	clampBoost(&w.StructuralImport)
	clampBoost(&w.Semantic)
	clampBoost(&w.WorkingSet)
	clampBoost(&w.ExactContent)
	clampBoost(&w.ExactFilename)
	clampBoost(&w.Graph)
	clampBoost(&w.ImplKind)
	clampBoost(&w.HeadingPathMatch)
	clampBoost(&w.LongChunkPenaltyMax)
	if w.TestDownrank <= 0.05 {
		w.TestDownrank = 0.05
	}
	if w.TestDownrank > 1.0 {
		w.TestDownrank = 1.0
	}
	if w.TinyChunkKeep <= 0.05 {
		w.TinyChunkKeep = 0.05
	}
	if w.TinyChunkKeep > 1.0 {
		w.TinyChunkKeep = 1.0
	}
	if w.LegacyPathKeep <= 0.05 {
		w.LegacyPathKeep = 0.05
	}
	if w.LegacyPathKeep > 1.0 {
		w.LegacyPathKeep = 1.0
	}
	if w.PathAffinityKeep <= 0.05 {
		w.PathAffinityKeep = 0.05
	}
	if w.PathAffinityKeep > 1.0 {
		w.PathAffinityKeep = 1.0
	}
}

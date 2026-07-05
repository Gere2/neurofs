package retrieval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

	// Penalties.
	LongChunkPenaltyMax float64 `json:"long_chunk_penalty_max"`
	TestDownrank        float64 `json:"test_downrank"`   // multiplicative keep-fraction in (0, 1]
	TinyChunkKeep       float64 `json:"tiny_chunk_keep"` // multiplicative keep-fraction in (0, 1] for sub-40-token chunks
	// LegacyPathKeep is the keep-fraction for chunks under compat/legacy
	// directories when the query does not ask for that surface. Neutral at
	// 1.0 and measured to stay there (2026-07-04): the 3-corpus tuner
	// declined 0.6/0.8, and a manual 0.3 probe left recall identical on
	// every shape while raising vue tokens 758 → 1154 — compat stubs were
	// no longer the binding constraint once impl_kind landed. Kept for
	// re-exploration as fixtures grow.
	LegacyPathKeep float64 `json:"legacy_path_keep"`
}

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
		LongChunkPenaltyMax:     4.0,
		TestDownrank:            0.72,
		// Neutral by default: downranking tiny chunks was A/B-tested on
		// three corpora (2026-07-04) and lost — click recall 66.7% → 53.3%
		// at keep=0.7, tokens up on every shape (tiny stubs are cheap).
		// The knob stays so the tuner can re-explore it as fixtures grow,
		// but only evidence should ever move it off 1.0.
		TinyChunkKeep:  1.0,
		LegacyPathKeep: 1.0,
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
	data, err := os.ReadFile(WeightsPath(repoRoot))
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
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("weights: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return fmt.Errorf("weights: marshal: %w", err)
	}
	return os.WriteFile(p, append(data, '\n'), 0o644)
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
}

package ranking

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Weights holds the file-level ranker's scoring weights. Like
// retrieval.Weights, the shipped defaults are the hand-calibrated constants
// this package always used; a per-repo .neurofs/ranking_weights.json (written
// by `neurofs learn tune-files --apply`) overrides them. The file ranker
// feeds the bundle path (taskflow → packager → gate G3), so these weights
// shape a different surface than the chunk-search weights.
type Weights struct {
	Filename        float64 `json:"filename"`
	Path            float64 `json:"path"`
	Symbol          float64 `json:"symbol"`
	Import          float64 `json:"import"`
	ImportExpansion float64 `json:"import_expansion"`
	LangBonus       float64 `json:"lang_bonus"`
	ContentMatch    float64 `json:"content_match"`
	EntryPoint      float64 `json:"entry_point"`
	DependencyMatch float64 `json:"dependency_match"`
	Focus           float64 `json:"focus"`
	Changed         float64 `json:"changed"`
	Semantic        float64 `json:"semantic"`
	RootDoc         float64 `json:"root_doc"`
}

// DefaultWeights returns the hand-calibrated values the package constants
// held before weights became tunable.
func DefaultWeights() Weights {
	return Weights{
		Filename:        3.0,
		Path:            1.5,
		Symbol:          2.5,
		Import:          1.0,
		ImportExpansion: 0.8,
		LangBonus:       0.3,
		ContentMatch:    0.5,
		EntryPoint:      1.5,
		DependencyMatch: 1.2,
		Focus:           4.0,
		Changed:         2.0,
		Semantic:        4.0,
		RootDoc:         3.5,
	}
}

// WeightsPath returns where tuned file-ranker weights live for repoRoot.
func WeightsPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".neurofs", "ranking_weights.json")
}

// LoadWeights reads tuned weights layered over defaults. Same contract as
// retrieval.LoadWeights: missing file is not an error, a malformed file
// returns defaults plus the error so retrieval keeps working while
// `neurofs learn status` can surface the problem.
func LoadWeights(repoRoot string) (Weights, bool, error) {
	w := DefaultWeights()
	data, err := os.ReadFile(WeightsPath(repoRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return w, false, nil
		}
		return w, false, fmt.Errorf("ranking weights: read: %w", err)
	}
	if err := json.Unmarshal(data, &w); err != nil {
		return DefaultWeights(), true, fmt.Errorf("ranking weights: parse %s: %w", WeightsPath(repoRoot), err)
	}
	w.Clamp()
	return w, true, nil
}

// SaveWeights persists w for repoRoot.
func SaveWeights(repoRoot string, w Weights) error {
	w.Clamp()
	p := WeightsPath(repoRoot)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return fmt.Errorf("ranking weights: mkdir: %w", err)
	}
	data, err := json.MarshalIndent(w, "", "  ")
	if err != nil {
		return fmt.Errorf("ranking weights: marshal: %w", err)
	}
	return os.WriteFile(p, append(data, '\n'), 0o644)
}

// Clamp bounds every weight to [0, 60].
func (w *Weights) Clamp() {
	clamp := func(v *float64) {
		if *v < 0 {
			*v = 0
		}
		if *v > 60 {
			*v = 60
		}
	}
	clamp(&w.Filename)
	clamp(&w.Path)
	clamp(&w.Symbol)
	clamp(&w.Import)
	clamp(&w.ImportExpansion)
	clamp(&w.LangBonus)
	clamp(&w.ContentMatch)
	clamp(&w.EntryPoint)
	clamp(&w.DependencyMatch)
	clamp(&w.Focus)
	clamp(&w.Changed)
	clamp(&w.Semantic)
	clamp(&w.RootDoc)
}

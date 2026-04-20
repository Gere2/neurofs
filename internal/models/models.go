// Package models defines the core data types for NeuroFS.
package models

import "time"

// Lang represents a supported programming language.
type Lang string

const (
	LangTypeScript Lang = "typescript"
	LangJavaScript Lang = "javascript"
	LangPython     Lang = "python"
	LangGo         Lang = "go"
	LangMarkdown   Lang = "markdown"
	LangJSON       Lang = "json"
	LangYAML       Lang = "yaml"
	LangUnknown    Lang = "unknown"
)

// Symbol is a named code element (function, class, constant, etc.).
type Symbol struct {
	Name string `json:"name"`
	Kind string `json:"kind"` // func, class, const, var, type, interface, export
	Line int    `json:"line"`
}

// FileRecord is the persisted representation of an indexed file.
type FileRecord struct {
	ID        int64     `json:"id"`
	Path      string    `json:"path"`     // absolute path
	RelPath   string    `json:"rel_path"` // relative to repo root
	Lang      Lang      `json:"lang"`
	Size      int64     `json:"size"`
	Lines     int       `json:"lines"`
	Symbols   []Symbol  `json:"symbols"`
	Imports   []string  `json:"imports"`
	Checksum  string    `json:"checksum"`
	IndexedAt time.Time `json:"indexed_at"`
}

// Representation controls how a file appears inside a bundle.
type Representation string

const (
	// RepFullCode includes the complete file content.
	RepFullCode Representation = "full_code"
	// RepSignature includes only symbol signatures.
	RepSignature Representation = "signature"
	// RepStructuralNote includes path, size, symbols, and imports as metadata.
	RepStructuralNote Representation = "structural_note"
	// RepSummaryPlaceholder marks a file that warrants a summary but none is
	// available yet (LLM summarisation is not wired in the MVP).
	RepSummaryPlaceholder Representation = "summary_placeholder"
)

// InclusionReason explains why a fragment was selected for a bundle.
type InclusionReason struct {
	Signal string  `json:"signal"` // filename_match, symbol_match, import_expansion, …
	Detail string  `json:"detail"`
	Weight float64 `json:"weight"`
}

// ContextFragment is one piece of context inside a bundle.
type ContextFragment struct {
	RelPath        string            `json:"rel_path"`
	Lang           Lang              `json:"lang"`
	Representation Representation    `json:"representation"`
	Content        string            `json:"content"`
	Tokens         int               `json:"tokens"`
	Score          float64           `json:"score"`
	Reasons        []InclusionReason `json:"reasons"`
}

// BundleStats records measurable properties of a bundle.
type BundleStats struct {
	FilesConsidered  int     `json:"files_considered"`
	FilesIncluded    int     `json:"files_included"`
	TokensUsed       int     `json:"tokens_used"`
	TokensBudget     int     `json:"tokens_budget"`
	CompressionRatio float64 `json:"compression_ratio"` // raw_tokens / tokens_used
}

// Bundle is the final auditable context package produced by NeuroFS.
type Bundle struct {
	Query     string            `json:"query"`
	Budget    int               `json:"budget"`
	Fragments []ContextFragment `json:"fragments"`
	Stats     BundleStats       `json:"stats"`
}

// ScoredFile is an intermediate result from the ranking stage.
type ScoredFile struct {
	Record  FileRecord
	Score   float64
	Reasons []InclusionReason
}

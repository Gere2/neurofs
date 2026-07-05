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
	LangRust       Lang = "rust"
	LangCpp        Lang = "cpp"
	LangJava       Lang = "java"
	LangRuby       Lang = "ruby"
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
	// RepExcerpt includes a subset of a file — the symbol blocks (functions,
	// classes, methods) whose names lexically match the query, with the rest
	// elided behind `// ... N lines omitted ...` markers. Used for top-ranked
	// files that are too large for full_code but where a signature would
	// throw away the very bodies the query is asking about.
	RepExcerpt Representation = "excerpt"
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
	StartLine      int               `json:"start_line,omitempty"`
	EndLine        int               `json:"end_line,omitempty"`
	ContentHash    string            `json:"content_hash,omitempty"`
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
//
// Repo, CommitSHA, GeneratedAt and BundleHash are populated by
// taskflow.EnrichBundle at the moment a bundle is about to be persisted.
// They are the audit identity fields a compliance/governance consumer
// needs to claim "this bundle is what we sent to the LLM at time T from
// commit X." BundleHash is the same content hash used by the audit
// replay path, so a record produced from this bundle and the bundle
// itself agree on identity.
type Bundle struct {
	Query       string            `json:"query"`
	Budget      int               `json:"budget"`
	Fragments   []ContextFragment `json:"fragments"`
	Stats       BundleStats       `json:"stats"`
	Repo        string            `json:"repo,omitempty"`
	CommitSHA   string            `json:"commit_sha,omitempty"`
	GeneratedAt time.Time         `json:"generated_at,omitempty"`
	BundleHash  string            `json:"bundle_hash,omitempty"`
}

// ScoredFile is an intermediate result from the ranking stage.
type ScoredFile struct {
	Record  FileRecord
	Score   float64
	Reasons []InclusionReason
}

// FileRelation represents a dependency relationship between two files.
type FileRelation struct {
	SourcePath string `json:"source_path"`
	TargetPath string `json:"target_path"`
	RelType    string `json:"rel_type"` // "import"
}

// Chunk represents a logic block of code within a file.
type Chunk struct {
	ID            int64     `json:"id"`
	FilePath      string    `json:"file_path"`
	ChunkID       string    `json:"chunk_id"`
	ParentID      string    `json:"parent_id"`
	Kind          string    `json:"kind"`
	Symbol        string    `json:"symbol"`
	StartLine     int       `json:"start_line"`
	EndLine       int       `json:"end_line"`
	Content       string    `json:"content"`
	ContentHash   string    `json:"content_hash"`
	ASTHash       string    `json:"ast_hash"`
	Calls         []string  `json:"calls,omitempty"`
	TokenEstimate int       `json:"token_estimate"`
	IndexedAt     time.Time `json:"indexed_at"`
}

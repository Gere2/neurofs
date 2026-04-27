package packager

import (
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
)

// TestExtractGoExcerpt_FilenameMatchesQuery is the regression for the
// G3 fixture "ranking-filename-match". The fact "filename_match" is a
// string literal inside the scoreFile body, NOT a top-level symbol —
// before the Go AST extractor it never reached the bundle (file was too
// big for full_code, signature drops string literals). With AST
// extraction we walk into scoreFile and the literal travels along.
//
// Terms here mirror what ranking.Tokenise produces for the fixture
// question "How does the ranker score filename matches and how is that
// weight defined?" — stop-words dropped, ≥3-char terms only. The match
// pathway: term "score" → symbol scoreFile (forward containment) →
// extract func body → "filename_match" travels along.
func TestExtractGoExcerpt_FilenameMatchesQuery(t *testing.T) {
	content := `package ranking

const weightFilename = 3.0

// scoreFile computes the score for one file given query terms.
func scoreFile(name string, terms []string) (float64, []Reason) {
	score := 0.0
	var reasons []Reason
	for _, t := range terms {
		if strings.Contains(name, t) {
			score += weightFilename
			reasons = append(reasons, Reason{Signal: "filename_match", Detail: t})
		}
	}
	return score, reasons
}

func somethingElse() int { return 0 }
`
	r := rec("internal/ranking/ranking.go", models.LangGo)

	terms := []string{"ranker", "score", "filename", "matches", "weight", "defined"}
	out, ok := extractGoExcerpt(r, content, terms)
	if !ok {
		t.Fatalf("expected an excerpt for the filename-match query")
	}
	for _, want := range []string{
		"weightFilename",   // const matched by "weight" or "filename"
		"scoreFile",        // func matched by "score"
		`"filename_match"`, // string literal inside scoreFile body
	} {
		if !strings.Contains(out, want) {
			t.Errorf("excerpt missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "somethingElse") {
		t.Errorf("excerpt leaked unrelated function:\n%s", out)
	}
}

// TestExtractGoExcerpt_StructFieldQueryFindsParentType is the regression
// for the G3 fixtures "packager-slack-upgrade" and
// "packager-excerpt-vs-signature". UpgradeWithSlack and PreferSignatures
// are FIELDS of Options — the regex parser never emitted them as
// symbols, so the heuristic matcher had no way to bring the Options
// block into the bundle. The Go path discovers field names from the
// StructType node and uses them for matching.
func TestExtractGoExcerpt_StructFieldQueryFindsParentType(t *testing.T) {
	content := `package packager

// Options configures bundle assembly.
type Options struct {
	Budget           int
	PreferSignatures bool
	UpgradeWithSlack bool
	QueryTerms       []string
}

func unrelated() {}
`
	r := rec("internal/packager/packager.go", models.LangGo)

	t.Run("UpgradeWithSlack brings the Options block", func(t *testing.T) {
		out, ok := extractGoExcerpt(r, content, []string{"upgradewithslack"})
		if !ok {
			t.Fatalf("expected excerpt; field name should match parent type")
		}
		if !strings.Contains(out, "UpgradeWithSlack bool") {
			t.Errorf("expected the matched field line; got:\n%s", out)
		}
		if !strings.Contains(out, "type Options") {
			t.Errorf("expected the carrier type header; got:\n%s", out)
		}
	})

	t.Run("PreferSignatures brings the Options block", func(t *testing.T) {
		out, ok := extractGoExcerpt(r, content, []string{"prefersignatures"})
		if !ok {
			t.Fatalf("expected excerpt for PreferSignatures field query")
		}
		if !strings.Contains(out, "PreferSignatures bool") {
			t.Errorf("expected the field; got:\n%s", out)
		}
		if strings.Contains(out, "func unrelated") {
			t.Errorf("excerpt leaked unrelated function:\n%s", out)
		}
	})
}

// TestExtractGoExcerpt_ExcerptVsSignatureQuery covers the third fixture.
// "excerpt vs signature" tokenises into multiple terms; the extractor
// should bring every matching const + func block. mergeOverlapping
// coalesces adjacent ones into a single contiguous range.
func TestExtractGoExcerpt_ExcerptVsSignatureQuery(t *testing.T) {
	content := `package models

type Representation string

const (
	RepFullCode  Representation = "full_code"
	RepExcerpt   Representation = "excerpt"
	RepSignature Representation = "signature"
)
`
	r := rec("internal/models/models.go", models.LangGo)

	out, ok := extractGoExcerpt(r, content, []string{"excerpt", "signature"})
	if !ok {
		t.Fatalf("expected excerpt for excerpt/signature query")
	}
	for _, want := range []string{"RepExcerpt", "RepSignature"} {
		if !strings.Contains(out, want) {
			t.Errorf("excerpt missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "RepFullCode") {
		// RepFullCode does NOT contain "excerpt" or "signature" — it
		// must not be dragged in. This guards per-spec granularity.
		t.Errorf("excerpt leaked RepFullCode (per-spec granularity broken):\n%s", out)
	}
}

// TestExtractGoExcerpt_MethodReceiverQualifiedName confirms that a
// query for the bare method name finds methods on a receiver. Mirrors
// the equivalent JS class-method test; without it we would silently
// skip every method on Go types.
func TestExtractGoExcerpt_MethodReceiverQualifiedName(t *testing.T) {
	content := `package storage

type DB struct{}

// Open returns a handle to the database.
func (d *DB) Open(path string) error {
	return nil
}

func (d *DB) unrelated() {}
`
	r := rec("internal/storage/storage.go", models.LangGo)

	out, ok := extractGoExcerpt(r, content, []string{"open"})
	if !ok {
		t.Fatalf("expected excerpt for method name query")
	}
	if !strings.Contains(out, "func (d *DB) Open(path string) error") {
		t.Errorf("expected the matched method body; got:\n%s", out)
	}
	if !strings.Contains(out, "Open returns a handle") {
		t.Errorf("doc comment should be included in the block; got:\n%s", out)
	}
	if strings.Contains(out, "unrelated") {
		t.Errorf("excerpt leaked unrelated method:\n%s", out)
	}
}

// TestExtractGoExcerpt_DocCommentsIncluded confirms doc comments above a
// matching decl are included — they are usually the most context-dense
// line of the file and cost very little to ship.
func TestExtractGoExcerpt_DocCommentsIncluded(t *testing.T) {
	content := `package x

// scoreFile is the workhorse of the ranker. It walks each query term
// against the file's name, symbols, and imports, summing weights.
func scoreFile() {}
`
	r := rec("x.go", models.LangGo)
	out, ok := extractGoExcerpt(r, content, []string{"score"})
	if !ok {
		t.Fatalf("expected excerpt")
	}
	if !strings.Contains(out, "is the workhorse of the ranker") {
		t.Errorf("doc comment dropped; got:\n%s", out)
	}
}

// TestExtractGoExcerpt_ParseFailureFallsBack confirms that broken Go
// returns ok=false rather than panicking or producing garbage. The
// caller's contract: a false ok means "fall back to signature".
func TestExtractGoExcerpt_ParseFailureFallsBack(t *testing.T) {
	content := "package broken\n\nfunc {{{ this is not go\n"
	r := rec("broken.go", models.LangGo)
	if _, ok := extractGoExcerpt(r, content, []string{"this"}); ok {
		t.Errorf("broken Go must return ok=false; caller falls back to signature")
	}
}

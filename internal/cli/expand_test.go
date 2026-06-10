package cli

import (
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/contextladder"
	"github.com/neuromfs/neuromfs/internal/models"
)

func TestParseExpandSpec(t *testing.T) {
	spec := contextladder.ParseSpec("src/auth.go:40-90")
	if spec.Path != "src/auth.go" || spec.StartLine != 40 || spec.EndLine != 90 {
		t.Fatalf("unexpected spec: %+v", spec)
	}
	plain := contextladder.ParseSpec("src/auth.go")
	if plain.Path != "src/auth.go" || plain.StartLine != 0 || plain.EndLine != 0 {
		t.Fatalf("unexpected plain spec: %+v", plain)
	}
}

func TestEffectiveExpandMode(t *testing.T) {
	mode, err := contextladder.EffectiveMode("auto", contextladder.Spec{Path: "src/auth.go"})
	if err != nil || mode != contextladder.ModeOutline {
		t.Fatalf("auto without range should be outline, got %q err=%v", mode, err)
	}
	mode, err = contextladder.EffectiveMode("auto", contextladder.Spec{Path: "src/auth.go", StartLine: 1, EndLine: 3})
	if err != nil || mode != contextladder.ModeExcerpt {
		t.Fatalf("auto with range should be excerpt, got %q err=%v", mode, err)
	}
}

func TestBuildExpandedExcerptVerifiesHashCoverage(t *testing.T) {
	rec := models.FileRecord{RelPath: "src/auth.go"}
	content := "a\nb\nc\nd\ne\n"
	hash := contextladder.SHA256Hex("b\nc\nd")
	chunks := []models.Chunk{{
		StartLine:   2,
		EndLine:     4,
		ContentHash: hash,
	}}
	out, err := contextladder.BuildExcerpt(rec, chunks, content, contextladder.Spec{
		Path:      "src/auth.go",
		StartLine: 2,
		EndLine:   4,
		Hash:      hash,
	})
	if err != nil {
		t.Fatalf("build excerpt: %v", err)
	}
	if out.StartLine != 2 || out.EndLine != 4 || out.ContentHash != hash || strings.TrimSpace(out.Content) != "b\nc\nd" {
		t.Fatalf("unexpected excerpt: %+v", out)
	}
	if _, err := contextladder.BuildExcerpt(rec, chunks, content, contextladder.Spec{
		Path:      "src/auth.go",
		StartLine: 1,
		EndLine:   3,
		Hash:      hash,
	}); err == nil {
		t.Fatalf("expected hash coverage error")
	}
}

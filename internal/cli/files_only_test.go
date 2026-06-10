package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/neuromfs/neuromfs/internal/models"
)

func TestWriteFilesOnlyTextRespectsLimitAndMinScore(t *testing.T) {
	ranked := []models.ScoredFile{
		filesOnlyScored("src/auth.go", 9.0),
		filesOnlyScored("src/session.go", 7.0),
		filesOnlyScored("src/noise.go", 1.0),
	}

	var buf bytes.Buffer
	err := writeFilesOnly(&buf, ranked, filesOnlyOptions{
		Limit:    1,
		MinScore: 5.0,
	})
	if err != nil {
		t.Fatalf("writeFilesOnly: %v", err)
	}
	got := buf.String()
	if !strings.Contains(got, "src/auth.go (score=9.00) - Reasons: filename_match: auth (+3.0)") {
		t.Fatalf("unexpected files-only text output:\n%s", got)
	}
	if strings.Contains(got, "src/session.go") || strings.Contains(got, "src/noise.go") {
		t.Fatalf("limit/min-score were not respected:\n%s", got)
	}
}

func TestWriteFilesOnlyJSONIncludesIndexedMetadata(t *testing.T) {
	ranked := []models.ScoredFile{filesOnlyScored("src/auth.go", 9.0)}

	var buf bytes.Buffer
	err := writeFilesOnly(&buf, ranked, filesOnlyOptions{JSON: true})
	if err != nil {
		t.Fatalf("writeFilesOnly: %v", err)
	}

	var entries []filesOnlyEntry
	if err := json.Unmarshal(buf.Bytes(), &entries); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, buf.String())
	}
	if len(entries) != 1 {
		t.Fatalf("expected one entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.Path != "src/auth.go" || entry.Lang != models.LangGo || entry.Lines != 42 || entry.Checksum != "hash-src/auth.go" {
		t.Fatalf("metadata missing from JSON entry: %+v", entry)
	}
	if len(entry.Symbols) != 1 || entry.Symbols[0].Name != "VerifyJWT" {
		t.Fatalf("symbols missing from JSON entry: %+v", entry.Symbols)
	}
	if len(entry.Reasons) != 1 || entry.Reasons[0].Signal != "filename_match" {
		t.Fatalf("reasons missing from JSON entry: %+v", entry.Reasons)
	}
}

func TestValidateFilesOnlyOptions(t *testing.T) {
	if err := validateFilesOnlyOptions(filesOnlyOptions{Limit: -1}); err == nil {
		t.Fatalf("negative limit should fail")
	}
	if err := validateFilesOnlyOptions(filesOnlyOptions{MinScore: -0.1}); err == nil {
		t.Fatalf("negative min-score should fail")
	}
}

func filesOnlyScored(path string, score float64) models.ScoredFile {
	return models.ScoredFile{
		Record: models.FileRecord{
			RelPath:  path,
			Lang:     models.LangGo,
			Lines:    42,
			Size:     1024,
			Checksum: "hash-" + path,
			Symbols: []models.Symbol{{
				Name: "VerifyJWT",
				Kind: "func",
				Line: 12,
			}},
			Imports: []string{"crypto"},
		},
		Score: score,
		Reasons: []models.InclusionReason{{
			Signal: "filename_match",
			Detail: "auth",
			Weight: 3.0,
		}},
	}
}

package output_test

import (
	"errors"
	"testing"

	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/output"
)

var errWriteFailed = errors.New("write failed")

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errWriteFailed
}

func TestWritersPropagateIOErrors(t *testing.T) {
	bundle := models.Bundle{
		Query: "q",
		Fragments: []models.ContextFragment{{
			RelPath: "main.go",
			Lang:    models.LangGo,
			Content: "package main",
		}},
	}

	for _, format := range []output.Format{
		output.FormatMarkdown,
		output.FormatText,
		output.FormatJSON,
		output.FormatClaude,
	} {
		t.Run(string(format), func(t *testing.T) {
			err := output.Write(failingWriter{}, bundle, format)
			if !errors.Is(err, errWriteFailed) {
				t.Fatalf("Write(%s) error = %v, want %v", format, err, errWriteFailed)
			}
		})
	}
}

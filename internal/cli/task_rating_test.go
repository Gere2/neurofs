package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/Gere2/neurofs/internal/quality"
)

type ratingFailWriter struct{}

func (ratingFailWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func TestPromptRatingHandlesEOFAndWriterErrors(t *testing.T) {
	var out strings.Builder
	rating, comment, err := promptRating(&out, strings.NewReader("y\nuseful"))
	if err != nil || rating != quality.RatingYes || comment != "useful" {
		t.Fatalf("rating=%q comment=%q err=%v", rating, comment, err)
	}

	rating, comment, err = promptRating(&out, strings.NewReader(""))
	if err != nil || rating != quality.RatingSkip || comment != "" {
		t.Fatalf("EOF should skip: rating=%q comment=%q err=%v", rating, comment, err)
	}

	if _, _, err := promptRating(ratingFailWriter{}, strings.NewReader("y\n")); err == nil {
		t.Fatal("writer failure was ignored")
	}
}

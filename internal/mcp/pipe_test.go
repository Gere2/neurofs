package mcp

import (
	"io"
	"testing"
)

func closePipeWriter(t *testing.T, writer *io.PipeWriter) {
	t.Helper()
	if err := writer.Close(); err != nil {
		t.Errorf("close pipe writer: %v", err)
	}
}

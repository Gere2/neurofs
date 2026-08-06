package taskflow

import (
	"context"

	"github.com/Gere2/neurofs/internal/models"
)

// LedgerWriter abstracts session logging within prompt-generation flows.
type LedgerWriter interface {
	AppendEntry(ctx context.Context, entry models.LedgerEntry) error
}

package memory

import (
	"context"

	"github.com/neuromfs/neuromfs/internal/models"
)

// Store abstracts the persistence layer for the ledger and active session tracking.
type Store interface {
	GetSessionID(ctx context.Context) (string, error)
	SaveSessionID(ctx context.Context, id string) error
	Append(ctx context.Context, entry models.LedgerEntry) error
	Read(ctx context.Context) ([]models.LedgerEntry, error)
	Search(ctx context.Context, term string) ([]models.LedgerEntry, error)
}

// Exporter formats session ledger entries into a portable file representation.
type Exporter interface {
	Export(sessionID string, entries []models.LedgerEntry) (string, error)
}

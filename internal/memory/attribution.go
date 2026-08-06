package memory

import (
	"context"
	"fmt"

	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/runid"
)

func bindLedgerEntry(ctx context.Context, entry *models.LedgerEntry) error {
	if entry == nil {
		return fmt.Errorf("memory: nil ledger entry")
	}
	attribution, err := runid.Bind(ctx, entry.Availability)
	if err != nil {
		return fmt.Errorf("memory: bind run identity: %w", err)
	}
	entry.Availability = attribution
	return nil
}

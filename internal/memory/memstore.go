package memory

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/neuromfs/neuromfs/internal/models"
)

// MemStore implements the Store interface in-memory for testing.
type MemStore struct {
	mu        sync.RWMutex
	sessionID string
	entries   []models.LedgerEntry
}

// NewMemStore constructs an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{
		sessionID: "test-session-mem",
	}
}

// GetSessionID returns the active session ID.
func (ms *MemStore) GetSessionID(ctx context.Context) (string, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()
	return ms.sessionID, nil
}

// SaveSessionID overrides the active session ID.
func (ms *MemStore) SaveSessionID(ctx context.Context, id string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.sessionID = id
	return nil
}

// Append logs a models.LedgerEntry to the in-memory store.
func (ms *MemStore) Append(ctx context.Context, entry models.LedgerEntry) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	ms.entries = append(ms.entries, entry)
	return nil
}

// Read returns logged in-memory entries, optionally filtered by sessionID.
func (ms *MemStore) Read(ctx context.Context, sessionID string) ([]models.LedgerEntry, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	var filtered []models.LedgerEntry
	for _, entry := range ms.entries {
		if sessionID == "" || entry.SessionID == sessionID {
			filtered = append(filtered, entry)
		}
	}
	return filtered, nil
}

// Search filters in-memory entries containing term (case-insensitive).
func (ms *MemStore) Search(ctx context.Context, term string) ([]models.LedgerEntry, error) {
	ms.mu.RLock()
	defer ms.mu.RUnlock()

	term = strings.ToLower(strings.TrimSpace(term))
	if term == "" {
		copied := make([]models.LedgerEntry, len(ms.entries))
		copy(copied, ms.entries)
		return copied, nil
	}

	var results []models.LedgerEntry
	for _, entry := range ms.entries {
		match := false
		if strings.Contains(strings.ToLower(entry.Query), term) ||
			strings.Contains(strings.ToLower(entry.Command), term) ||
			strings.Contains(strings.ToLower(entry.Outcome), term) ||
			strings.Contains(strings.ToLower(entry.Notes), term) ||
			strings.Contains(strings.ToLower(entry.SessionID), term) ||
			strings.Contains(strings.ToLower(entry.BundleHash), term) {
			match = true
		} else {
			for _, file := range entry.Files {
				if strings.Contains(strings.ToLower(file), term) {
					match = true
					break
				}
			}
		}

		if match {
			results = append(results, entry)
		}
	}
	return results, nil
}

// Prune removes entries older than olderThan from memory.
func (ms *MemStore) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	cutoff := time.Now().Add(-olderThan)
	var kept []models.LedgerEntry
	var count int64
	for _, entry := range ms.entries {
		if entry.Timestamp.Before(cutoff) {
			count++
		} else {
			kept = append(kept, entry)
		}
	}
	ms.entries = kept
	return count, nil
}

package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/neuromfs/neuromfs/internal/models"
)

const sessionDuration = 8 * time.Hour

// Manager coordinates stores and exporters to handle session logs.
type Manager struct {
	store     Store
	exporters map[string]Exporter
	mu        sync.RWMutex
}

// New constructs a Manager wrapping a Store.
func New(store Store) *Manager {
	m := &Manager{
		store:     store,
		exporters: make(map[string]Exporter),
	}
	m.RegisterExporter("session_timeline", TimelineExporter{})
	m.RegisterExporter("agents", AgentsExporter{})
	m.RegisterExporter("markdown", MarkdownExporter{})
	// Compatibility tags
	m.RegisterExporter("claude", TimelineExporter{})
	return m
}

// RegisterExporter adds a template generator to the registry.
func (m *Manager) RegisterExporter(format string, exp Exporter) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.exporters[strings.ToLower(strings.TrimSpace(format))] = exp
}

// GetSessionID resolves the active session ID.
func (m *Manager) GetSessionID(ctx context.Context) (string, error) {
	return m.store.GetSessionID(ctx)
}

// AppendEntry writes a models.LedgerEntry to the store.
func (m *Manager) AppendEntry(ctx context.Context, entry models.LedgerEntry) error {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	if entry.SessionID == "" {
		sid, err := m.store.GetSessionID(ctx)
		if err != nil {
			return err
		}
		entry.SessionID = sid
	}
	return m.store.Append(ctx, entry)
}

// SearchEntries filters entries by term.
func (m *Manager) SearchEntries(ctx context.Context, term string) ([]models.LedgerEntry, error) {
	return m.store.Search(ctx, term)
}

// ExportEntries produces a formatted markdown export for the specified session (or active session if sessionID is empty).
func (m *Manager) ExportEntries(ctx context.Context, sessionID string, format string) (string, error) {
	if sessionID == "" {
		var err error
		sessionID, err = m.store.GetSessionID(ctx)
		if err != nil {
			return "", err
		}
	}

	sessionEntries, err := m.store.Read(ctx, sessionID)
	if err != nil {
		return "", err
	}

	m.mu.RLock()
	exporter, ok := m.exporters[strings.ToLower(strings.TrimSpace(format))]
	m.mu.RUnlock()

	if !ok {
		return "", fmt.Errorf("unsupported export format: %s", format)
	}

	return exporter.Export(sessionID, sessionEntries)
}

// Prune removes entries older than olderThan from the store.
func (m *Manager) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
	return m.store.Prune(ctx, olderThan)
}

// Compatibility package-level APIs (delegating to SqliteStore automatically)
// This preserves backwards compatibility for any legacy callers.

func GetSessionID(repoRoot string) string {
	m := New(NewSqliteStore(repoRoot))
	id, _ := m.GetSessionID(context.Background())
	return id
}

func AppendEntry(repoRoot string, entry models.LedgerEntry) error {
	m := New(NewSqliteStore(repoRoot))
	return m.AppendEntry(context.Background(), entry)
}

func ReadEntries(repoRoot string) ([]models.LedgerEntry, error) {
	fs := NewSqliteStore(repoRoot)
	return fs.Read(context.Background(), "")
}

func SearchEntries(repoRoot string, term string) ([]models.LedgerEntry, error) {
	m := New(NewSqliteStore(repoRoot))
	return m.SearchEntries(context.Background(), term)
}

func ExportEntries(repoRoot string, format string) (string, error) {
	m := New(NewSqliteStore(repoRoot))
	if format == "claude" {
		format = "session_timeline"
	}
	return m.ExportEntries(context.Background(), "", format)
}

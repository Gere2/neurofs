package memory

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/neuromfs/neuromfs/internal/models"
	_ "modernc.org/sqlite"
)

// SqliteStore implements the Store interface using local SQLite.
type SqliteStore struct {
	repoRoot string
	dbPath   string
	mu       sync.Mutex // guards session.txt access
}

// NewSqliteStore constructs a SqliteStore rooted at the repository.
func NewSqliteStore(repoRoot string) *SqliteStore {
	dbPath := filepath.Join(repoRoot, ".neurofs", "ledger.db")
	return &SqliteStore{
		repoRoot: repoRoot,
		dbPath:   dbPath,
	}
}

// openDB opens the SQLite connection, sets pragmas, and ensures the table exists.
func (s *SqliteStore) openDB(ctx context.Context) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(s.dbPath), 0755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite", s.dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	// WAL mode and busy timeout are critical for process-level concurrency safety
	pragmas := []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %s: %w", p, err)
		}
	}

	schema := `
	CREATE TABLE IF NOT EXISTS session_ledger (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp TEXT NOT NULL,
		session_id TEXT NOT NULL,
		query TEXT,
		bundle_hash TEXT,
		files TEXT,
		command TEXT,
		outcome TEXT,
		notes TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_ledger_session_timestamp ON session_ledger (session_id, timestamp DESC);
	`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}

	return db, nil
}

// GetSessionID resolves the current active session ID.
func (s *SqliteStore) GetSessionID(ctx context.Context) (string, error) {
	if envID := os.Getenv("NEUROFS_SESSION_ID"); envID != "" {
		return strings.TrimSpace(envID), nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sessionFile := filepath.Join(s.repoRoot, ".neurofs", "session.txt")
	if info, err := os.Stat(sessionFile); err == nil {
		if time.Since(info.ModTime()) < sessionDuration {
			f, err := os.Open(sessionFile)
			if err == nil {
				defer f.Close()
				data := make([]byte, 256)
				n, _ := io.ReadAtLeast(f, data, 1)
				id := strings.TrimSpace(string(data[:n]))
				if id != "" {
					return id, nil
				}
			}
		}
	}

	// Generate a secure, random session ID
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}
	newID := fmt.Sprintf("sess-%s", hex.EncodeToString(b))

	_ = os.MkdirAll(filepath.Dir(sessionFile), 0755)
	if err := os.WriteFile(sessionFile, []byte(newID), 0644); err != nil {
		return "", fmt.Errorf("write session file: %w", err)
	}
	return newID, nil
}

// SaveSessionID writes a specific session ID to session.txt.
func (s *SqliteStore) SaveSessionID(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	sessionFile := filepath.Join(s.repoRoot, ".neurofs", "session.txt")
	_ = os.MkdirAll(filepath.Dir(sessionFile), 0755)
	return os.WriteFile(sessionFile, []byte(id), 0644)
}

// Append logs a models.LedgerEntry to SQLite.
func (s *SqliteStore) Append(ctx context.Context, entry models.LedgerEntry) error {
	db, err := s.openDB(ctx)
	if err != nil {
		return err
	}
	defer db.Close()

	// Reset sliding session window by touching session.txt
	sessionFile := filepath.Join(s.repoRoot, ".neurofs", "session.txt")
	if _, err := os.Stat(sessionFile); err == nil {
		now := time.Now()
		_ = os.Chtimes(sessionFile, now, now)
	}

	filesJSON := "[]"
	if len(entry.Files) > 0 {
		b, err := json.Marshal(entry.Files)
		if err == nil {
			filesJSON = string(b)
		}
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO session_ledger (timestamp, session_id, query, bundle_hash, files, command, outcome, notes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`,
		entry.Timestamp.UTC().Format(time.RFC3339),
		entry.SessionID,
		entry.Query,
		entry.BundleHash,
		filesJSON,
		entry.Command,
		entry.Outcome,
		entry.Notes,
	)
	if err != nil {
		return fmt.Errorf("insert ledger entry: %w", err)
	}
	return nil
}

// Read parses all entries from SQLite.
func (s *SqliteStore) Read(ctx context.Context) ([]models.LedgerEntry, error) {
	db, err := s.openDB(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx, `
		SELECT timestamp, session_id, query, bundle_hash, files, command, outcome, notes
		FROM session_ledger
		ORDER BY timestamp ASC
	`)
	if err != nil {
		return nil, fmt.Errorf("query ledger entries: %w", err)
	}
	defer rows.Close()

	var entries []models.LedgerEntry
	for rows.Next() {
		var (
			entry     models.LedgerEntry
			tsStr     string
			filesJSON string
		)
		err := rows.Scan(
			&tsStr,
			&entry.SessionID,
			&entry.Query,
			&entry.BundleHash,
			&filesJSON,
			&entry.Command,
			&entry.Outcome,
			&entry.Notes,
		)
		if err != nil {
			return nil, fmt.Errorf("scan ledger entry: %w", err)
		}
		if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
			entry.Timestamp = t.Local()
		}
		if filesJSON != "" {
			_ = json.Unmarshal([]byte(filesJSON), &entry.Files)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

// Search filters ledger entries containing term (case-insensitive).
func (s *SqliteStore) Search(ctx context.Context, term string) ([]models.LedgerEntry, error) {
	db, err := s.openDB(ctx)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	term = strings.ToLower(strings.TrimSpace(term))
	if term == "" {
		return s.Read(ctx)
	}

	likeTerm := "%" + term + "%"
	rows, err := db.QueryContext(ctx, `
		SELECT timestamp, session_id, query, bundle_hash, files, command, outcome, notes
		FROM session_ledger
		WHERE LOWER(query) LIKE ?
		   OR LOWER(command) LIKE ?
		   OR LOWER(outcome) LIKE ?
		   OR LOWER(notes) LIKE ?
		   OR LOWER(session_id) LIKE ?
		   OR LOWER(files) LIKE ?
		ORDER BY timestamp ASC
	`, likeTerm, likeTerm, likeTerm, likeTerm, likeTerm, likeTerm)
	if err != nil {
		return nil, fmt.Errorf("search ledger entries: %w", err)
	}
	defer rows.Close()

	var entries []models.LedgerEntry
	for rows.Next() {
		var (
			entry     models.LedgerEntry
			tsStr     string
			filesJSON string
		)
		err := rows.Scan(
			&tsStr,
			&entry.SessionID,
			&entry.Query,
			&entry.BundleHash,
			&filesJSON,
			&entry.Command,
			&entry.Outcome,
			&entry.Notes,
		)
		if err != nil {
			return nil, fmt.Errorf("scan ledger entry: %w", err)
		}
		if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
			entry.Timestamp = t.Local()
		}
		if filesJSON != "" {
			_ = json.Unmarshal([]byte(filesJSON), &entry.Files)
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

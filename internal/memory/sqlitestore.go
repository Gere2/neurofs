package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Gere2/neurofs/internal/atomicfile"
	"github.com/Gere2/neurofs/internal/models"
	_ "modernc.org/sqlite"
)

// SqliteStore implements the Store interface using local SQLite.
type SqliteStore struct {
	repoRoot string
	dbPath   string
	mu       sync.Mutex // guards session.txt access
}

const autoPruneLockStaleAfter = time.Hour

func closeDB(db *sql.DB) {
	_ = db.Close()
}

func closeSQLRows(rows *sql.Rows) {
	_ = rows.Close()
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
	if err := os.MkdirAll(filepath.Dir(s.dbPath), 0o755); err != nil {
		return nil, fmt.Errorf("create db dir: %w", err)
	}

	db, err := sql.Open("sqlite", s.dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	// WAL mode and busy timeout are critical for process-level concurrency safety
	pragmas := []string{
		"PRAGMA busy_timeout = 5000",
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA foreign_keys = ON",
	}
	for _, p := range pragmas {
		if _, err := db.ExecContext(ctx, p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("pragma %s: %w", p, err)
		}
	}

	schema := `
	CREATE TABLE IF NOT EXISTS session_ledger (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp TEXT NOT NULL,
		session_id TEXT NOT NULL,
		run_id TEXT NOT NULL DEFAULT '',
		run_correlation TEXT NOT NULL DEFAULT '',
		run_correlation_reason TEXT NOT NULL DEFAULT '',
		query TEXT,
		bundle_path TEXT NOT NULL DEFAULT '',
		bundle_hash TEXT,
		files TEXT,
		command TEXT,
		outcome TEXT,
		notes TEXT
	);
	CREATE INDEX IF NOT EXISTS idx_ledger_session_timestamp ON session_ledger (session_id, timestamp DESC);
	`
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// Existing ledger.db files predate run correlation. Additive columns with
	// empty defaults preserve every legacy row while allowing new writes to
	// distinguish legacy, unavailable, and correlated entries.
	migrations := []struct {
		column string
		sql    string
	}{
		{"run_id", `ALTER TABLE session_ledger ADD COLUMN run_id TEXT NOT NULL DEFAULT ''`},
		{"run_correlation", `ALTER TABLE session_ledger ADD COLUMN run_correlation TEXT NOT NULL DEFAULT ''`},
		{"run_correlation_reason", `ALTER TABLE session_ledger ADD COLUMN run_correlation_reason TEXT NOT NULL DEFAULT ''`},
		{"bundle_path", `ALTER TABLE session_ledger ADD COLUMN bundle_path TEXT NOT NULL DEFAULT ''`},
	}
	for _, migration := range migrations {
		if err := ensureLedgerColumn(ctx, db, migration.column, migration.sql); err != nil {
			_ = db.Close()
			return nil, err
		}
	}
	if _, err := db.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_ledger_run_bundle ON session_ledger (run_id, bundle_path, bundle_hash)`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("create run-bundle index: %w", err)
	}

	return db, nil
}

func ensureLedgerColumn(ctx context.Context, db *sql.DB, column, alter string) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(session_ledger)`)
	if err != nil {
		return fmt.Errorf("inspect session_ledger schema: %w", err)
	}
	found := false
	for rows.Next() {
		var (
			cid        int
			name       string
			dataType   string
			notNull    int
			defaultV   sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &dataType, &notNull, &defaultV, &primaryKey); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan session_ledger schema: %w", err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read session_ledger schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close session_ledger schema rows: %w", err)
	}
	if found {
		return nil
	}
	if _, err := db.ExecContext(ctx, alter); err != nil {
		return fmt.Errorf("migrate session_ledger column %s: %w", column, err)
	}
	return nil
}

// GetSessionID resolves the current active session ID.
func (s *SqliteStore) GetSessionID(ctx context.Context) (string, error) {
	if envID, configured, err := sessionIDFromEnv(); configured {
		return envID, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	sessionFile := filepath.Join(s.repoRoot, ".neurofs", "session.txt")
	if id, fresh, err := loadFreshSessionID(sessionFile); err != nil {
		return "", err
	} else if fresh {
		return id, nil
	}

	newID, err := newSessionID()
	if err != nil {
		return "", err
	}
	if _, err := saveSessionIDFile(sessionFile, newID); err != nil {
		return "", err
	}
	return newID, nil
}

// SaveSessionID writes a specific session ID to session.txt.
func (s *SqliteStore) SaveSessionID(ctx context.Context, id string) error {
	id, err := normalizeSessionID(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	sessionFile := filepath.Join(s.repoRoot, ".neurofs", "session.txt")
	_, err = saveSessionIDFile(sessionFile, id)
	return err
}

// Append logs a models.LedgerEntry to SQLite.
func (s *SqliteStore) Append(ctx context.Context, entry models.LedgerEntry) error {
	if err := bindLedgerEntry(ctx, &entry); err != nil {
		return err
	}
	db, err := s.openDB(ctx)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			_ = db.Close()
		}
	}()

	// Reset sliding session window by touching session.txt
	sessionFile := filepath.Join(s.repoRoot, ".neurofs", "session.txt")
	touchSessionFile(sessionFile)

	filesJSON := "[]"
	if len(entry.Files) > 0 {
		b, err := json.Marshal(entry.Files)
		if err == nil {
			filesJSON = string(b)
		}
	}

	_, err = db.ExecContext(ctx, `
		INSERT INTO session_ledger (
			timestamp, session_id, run_id, run_correlation, run_correlation_reason,
			query, bundle_path, bundle_hash, files, command, outcome, notes
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		entry.Timestamp.UTC().Format(time.RFC3339),
		entry.SessionID,
		entry.RunID.String(),
		string(entry.Correlation),
		entry.Reason,
		entry.Query,
		entry.BundlePath,
		entry.BundleHash,
		filesJSON,
		entry.Command,
		entry.Outcome,
		entry.Notes,
	)
	if err != nil {
		return fmt.Errorf("insert ledger entry: %w", err)
	}

	// Close the append connection before maintenance so VACUUM never competes
	// with an idle handle held by this same command.
	_ = db.Close()
	closed = true
	_ = s.checkAutoPrune(ctx)
	return nil
}

// Read parses entries from SQLite, optionally filtered by sessionID.
func (s *SqliteStore) Read(ctx context.Context, sessionID string) ([]models.LedgerEntry, error) {
	db, err := s.openDB(ctx)
	if err != nil {
		return nil, err
	}
	defer closeDB(db)

	query := `
		SELECT timestamp, session_id, run_id, run_correlation, run_correlation_reason,
		       query, bundle_path, bundle_hash, files, command, outcome, notes
		FROM session_ledger
	`
	var args []any
	if sessionID != "" {
		query += " WHERE session_id = ?"
		args = append(args, sessionID)
	}
	query += " ORDER BY timestamp ASC"

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query ledger entries: %w", err)
	}
	defer closeSQLRows(rows)

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
			&entry.RunID,
			&entry.Correlation,
			&entry.Reason,
			&entry.Query,
			&entry.BundlePath,
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
	defer closeDB(db)

	term = strings.ToLower(strings.TrimSpace(term))
	if term == "" {
		return s.Read(ctx, "")
	}

	rows, err := db.QueryContext(ctx, `
		SELECT timestamp, session_id, run_id, run_correlation, run_correlation_reason,
		       query, bundle_path, bundle_hash, files, command, outcome, notes
		FROM session_ledger
		WHERE INSTR(LOWER(COALESCE(query, '')), ?) > 0
		   OR INSTR(LOWER(COALESCE(command, '')), ?) > 0
		   OR INSTR(LOWER(COALESCE(outcome, '')), ?) > 0
		   OR INSTR(LOWER(COALESCE(notes, '')), ?) > 0
		   OR INSTR(LOWER(COALESCE(session_id, '')), ?) > 0
		   OR INSTR(LOWER(COALESCE(run_id, '')), ?) > 0
		   OR INSTR(LOWER(COALESCE(run_correlation, '')), ?) > 0
		   OR INSTR(LOWER(COALESCE(run_correlation_reason, '')), ?) > 0
		   OR INSTR(LOWER(COALESCE(bundle_path, '')), ?) > 0
		   OR INSTR(LOWER(COALESCE(bundle_hash, '')), ?) > 0
		   OR INSTR(LOWER(COALESCE(files, '')), ?) > 0
		ORDER BY timestamp ASC
	`, term, term, term, term, term, term, term, term, term, term, term)
	if err != nil {
		return nil, fmt.Errorf("search ledger entries: %w", err)
	}
	defer closeSQLRows(rows)

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
			&entry.RunID,
			&entry.Correlation,
			&entry.Reason,
			&entry.Query,
			&entry.BundlePath,
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

// Prune removes entries older than olderThan from SQLite and runs VACUUM.
func (s *SqliteStore) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
	db, err := s.openDB(ctx)
	if err != nil {
		return 0, err
	}
	defer closeDB(db)

	cutoff := time.Now().Add(-olderThan).UTC().Format(time.RFC3339)
	res, err := db.ExecContext(ctx, `
		DELETE FROM session_ledger
		WHERE timestamp < ?
	`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("delete old ledger entries: %w", err)
	}

	rowsAffected, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("get rows affected: %w", err)
	}

	if rowsAffected > 0 {
		_, _ = db.ExecContext(ctx, "VACUUM")
	}

	return rowsAffected, nil
}

// checkAutoPrune performs at most one synchronous retention pass per day.
// Append closes its write handle first, and the cross-process lock prevents
// concurrent CLI invocations from all attempting VACUUM.
func (s *SqliteStore) checkAutoPrune(ctx context.Context) error {
	pruneFile := filepath.Join(s.repoRoot, ".neurofs", "last_prune_sqlite.txt")
	if info, err := os.Stat(pruneFile); err == nil {
		if time.Since(info.ModTime()) < 24*time.Hour {
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(pruneFile), 0o755); err != nil {
		return fmt.Errorf("create auto-prune dir: %w", err)
	}
	lockPath := filepath.Join(filepath.Dir(pruneFile), "last_prune_sqlite.lock")
	lock, acquired, err := acquireAutoPruneLock(lockPath)
	if err != nil {
		return err
	}
	if !acquired {
		return nil
	}
	defer func() {
		_ = lock.Close()
		_ = os.Remove(lockPath)
	}()

	// A process may have completed pruning between our first marker check and
	// acquiring the lock.
	if info, err := os.Stat(pruneFile); err == nil && time.Since(info.ModTime()) < 24*time.Hour {
		return nil
	}
	if _, err := s.Prune(ctx, 30*24*time.Hour); err != nil {
		return err
	}
	if err := atomicfile.WriteFile(
		pruneFile,
		[]byte(time.Now().UTC().Format(time.RFC3339)),
		0o644,
	); err != nil {
		return fmt.Errorf("write auto-prune marker: %w", err)
	}
	return nil
}

func acquireAutoPruneLock(lockPath string) (*os.File, bool, error) {
	for attempt := 0; attempt < 2; attempt++ {
		lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			return lock, true, nil
		}
		if !os.IsExist(err) {
			return nil, false, fmt.Errorf("acquire auto-prune lock: %w", err)
		}

		info, statErr := os.Lstat(lockPath)
		if os.IsNotExist(statErr) {
			continue
		}
		if statErr != nil {
			return nil, false, fmt.Errorf("inspect auto-prune lock: %w", statErr)
		}
		// Only a regular lock file older than the conservative threshold is
		// recoverable. Never remove directories, links, future-dated locks, or
		// a lock that could belong to a live prune.
		age := time.Since(info.ModTime())
		if !info.Mode().IsRegular() || age < autoPruneLockStaleAfter {
			return nil, false, nil
		}
		if err := os.Remove(lockPath); err != nil && !os.IsNotExist(err) {
			return nil, false, fmt.Errorf("remove stale auto-prune lock: %w", err)
		}
	}
	return nil, false, nil
}

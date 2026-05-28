package storage

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/neuromfs/neuromfs/internal/models"
)

func newTempDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestOpenEnablesWAL(t *testing.T) {
	db := newTempDB(t)

	var mode string
	if err := db.db.QueryRow("PRAGMA journal_mode").Scan(&mode); err != nil {
		t.Fatalf("journal_mode: %v", err)
	}
	if mode != "wal" {
		t.Fatalf("journal_mode = %q, want %q", mode, "wal")
	}

	var timeout int
	if err := db.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if timeout < 5000 {
		t.Fatalf("busy_timeout = %d, want >= 5000", timeout)
	}
}

// TestConcurrentWritersDoNotFail reproduces the scenario the stress agent
// found broken: two independent DB handles writing to the same file. Before
// WAL + busy_timeout, this returned SQLITE_BUSY in seconds; after, the
// writers serialize cleanly.
func TestConcurrentWritersDoNotFail(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "concurrent.db")

	const writers = 4
	const opsPerWriter = 25

	var wg sync.WaitGroup
	errCh := make(chan error, writers)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			db, err := Open(dbPath)
			if err != nil {
				errCh <- err
				return
			}
			defer db.Close()
			for i := 0; i < opsPerWriter; i++ {
				// unique path per (writer, op) so UNIQUE(path) holds
				p := filepath.Join("/fake", string(rune('a'+id)), string(rune('a'+i%26)))
				if err := db.UpsertFile(models.FileRecord{
					Path:      p,
					RelPath:   p,
					Lang:      models.Lang("go"),
					Size:      int64(i),
					Lines:     i,
					Checksum:  "x",
					IndexedAt: time.Now(),
				}); err != nil {
					errCh <- err
					return
				}
			}
		}(w)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent upsert failed: %v", err)
	}
}

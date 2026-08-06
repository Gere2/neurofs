package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gere2/neurofs/internal/models"
)

func TestGetSessionID(t *testing.T) {
	tempDir := t.TempDir()
	fs := NewSqliteStore(tempDir)
	ctx := context.Background()

	// 1. Env Var override
	t.Setenv("NEUROFS_SESSION_ID", "env-session-123")

	id, err := fs.GetSessionID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id != "env-session-123" {
		t.Errorf("expected env override 'env-session-123', got %q", id)
	}

	// Unset to test file-based
	if err := os.Unsetenv("NEUROFS_SESSION_ID"); err != nil {
		t.Fatalf("unset session override: %v", err)
	}

	// 2. Fresh session creation
	id1, err := fs.GetSessionID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id1, "sess-") {
		t.Errorf("expected session prefix 'sess-', got %q", id1)
	}
	sessionFile := filepath.Join(tempDir, ".neurofs", "session.txt")
	sessionInfo, err := os.Lstat(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if sessionInfo.Mode()&os.ModeSymlink != 0 || !sessionInfo.Mode().IsRegular() {
		t.Fatalf("generated session state is not a regular file: %v", sessionInfo.Mode())
	}
	if sessionInfo.Mode().Perm() != 0o600 {
		t.Fatalf("generated session permissions = %o, want 600", sessionInfo.Mode().Perm())
	}

	// 3. Cache hit on fresh session
	id2, err := fs.GetSessionID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("expected cached session ID %q, got %q", id1, id2)
	}

	// 4. Stale session expiration (by modifying file mtime to >8 hours ago)
	past := time.Now().Add(-9 * time.Hour)
	err = os.Chtimes(sessionFile, past, past)
	if err != nil {
		t.Fatal(err)
	}

	id3, err := fs.GetSessionID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id3 {
		t.Errorf("expected new session ID after expiration, but got same %q", id3)
	}
}

func TestSessionIDValidation(t *testing.T) {
	for _, raw := range []string{"", "   ", "bad\nsession", "bad\rsession"} {
		t.Run("env_"+strings.ReplaceAll(raw, "\n", "newline"), func(t *testing.T) {
			t.Setenv("NEUROFS_SESSION_ID", raw)
			if _, err := NewSqliteStore(t.TempDir()).GetSessionID(context.Background()); err == nil {
				t.Fatalf("expected invalid environment session ID %q to fail", raw)
			}
		})
	}

	stores := []struct {
		name string
		new  func(string) Store
	}{
		{name: "sqlite", new: func(root string) Store { return NewSqliteStore(root) }},
		{name: "file", new: func(root string) Store { return NewFileStore(root) }},
	}
	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) {
			repo := t.TempDir()
			store := tc.new(repo)
			for _, invalid := range []string{"", " ", "two\nlines"} {
				if err := store.SaveSessionID(context.Background(), invalid); err == nil {
					t.Fatalf("SaveSessionID(%q) unexpectedly succeeded", invalid)
				}
			}
			if err := store.SaveSessionID(context.Background(), "  manual-session  "); err != nil {
				t.Fatalf("save valid session: %v", err)
			}
			id, err := store.GetSessionID(context.Background())
			if err != nil {
				t.Fatalf("get saved session: %v", err)
			}
			if id != "manual-session" {
				t.Fatalf("saved session ID = %q, want trimmed value", id)
			}
			info, err := os.Lstat(filepath.Join(repo, ".neurofs", "session.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Fatalf("session permissions = %o, want 600", info.Mode().Perm())
			}
		})
	}
}

func TestSessionFileRejectsSymlinkAndSaveReplacesItSafely(t *testing.T) {
	repo := t.TempDir()
	stateDir := filepath.Join(repo, ".neurofs")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(target, []byte("outside-session"), 0o600); err != nil {
		t.Fatal(err)
	}
	sessionFile := filepath.Join(stateDir, "session.txt")
	if err := os.Symlink(target, sessionFile); err != nil {
		t.Fatal(err)
	}

	store := NewSqliteStore(repo)
	if _, err := store.GetSessionID(context.Background()); err == nil {
		t.Fatal("expected symlink session file to be rejected")
	}
	if err := store.SaveSessionID(context.Background(), "safe-session"); err != nil {
		t.Fatalf("atomic save over symlink: %v", err)
	}
	info, err := os.Lstat(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		t.Fatalf("session path was not replaced with a regular file: %v", info.Mode())
	}
	outside, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(outside) != "outside-session" {
		t.Fatalf("symlink target was modified: %q", outside)
	}
}

func TestAppendAndReadEntries(t *testing.T) {
	tempDir := t.TempDir()
	fs := NewSqliteStore(tempDir)
	ctx := context.Background()
	m := New(fs)

	entry1 := models.LedgerEntry{
		Query:      "test query 1",
		BundleHash: "hash123",
		Files:      []string{"file1.go", "file2.go"},
		Outcome:    "success",
		Notes:      "auto-logged",
	}

	err := m.AppendEntry(ctx, entry1)
	if err != nil {
		t.Fatalf("failed to append entry: %v", err)
	}

	entries, err := fs.Read(ctx, "")
	if err != nil {
		t.Fatalf("failed to read entries: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	if entries[0].Query != entry1.Query || entries[0].BundleHash != entry1.BundleHash || entries[0].Outcome != entry1.Outcome {
		t.Errorf("entry mismatch: %+v vs %+v", entries[0], entry1)
	}

	// Test Search with pre-filtering
	matches, err := m.SearchEntries(ctx, "query 1")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Errorf("expected 1 search match, got %d", len(matches))
	}

	matches2, err := m.SearchEntries(ctx, "nonexistent")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches2) != 0 {
		t.Errorf("expected 0 search matches, got %d", len(matches2))
	}

	// SQLite search must cover bundle hashes and treat SQL wildcard
	// characters as ordinary text, matching the in-memory implementation.
	err = m.AppendEntry(ctx, models.LedgerEntry{
		Query:      "second entry",
		BundleHash: "bundle%_ABC",
	})
	if err != nil {
		t.Fatal(err)
	}
	hashMatches, err := m.SearchEntries(ctx, "HASH123")
	if err != nil {
		t.Fatal(err)
	}
	if len(hashMatches) != 1 || hashMatches[0].BundleHash != "hash123" {
		t.Fatalf("bundle hash search = %+v, want hash123 entry", hashMatches)
	}
	literalMatches, err := m.SearchEntries(ctx, "%_")
	if err != nil {
		t.Fatal(err)
	}
	if len(literalMatches) != 1 || literalMatches[0].BundleHash != "bundle%_ABC" {
		t.Fatalf("literal wildcard search = %+v, want bundle%%_ABC entry", literalMatches)
	}
}

func TestPersistentStoresShareLiteralSearchSemantics(t *testing.T) {
	stores := []struct {
		name string
		new  func(string) Store
	}{
		{name: "sqlite", new: func(root string) Store { return NewSqliteStore(root) }},
		{name: "file", new: func(root string) Store { return NewFileStore(root) }},
	}
	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.new(t.TempDir())
			ctx := context.Background()
			entries := []models.LedgerEntry{
				{Timestamp: time.Now(), Query: "first", BundleHash: "bundle%_ABC"},
				{Timestamp: time.Now(), Query: "second", BundleHash: "plain"},
			}
			for _, entry := range entries {
				if err := store.Append(ctx, entry); err != nil {
					t.Fatalf("append: %v", err)
				}
			}
			matches, err := store.Search(ctx, "%_")
			if err != nil {
				t.Fatalf("literal search: %v", err)
			}
			if len(matches) != 1 || matches[0].BundleHash != "bundle%_ABC" {
				t.Fatalf("literal search = %+v, want bundle%%_ABC entry", matches)
			}
		})
	}
}

func TestFileStoreLedgerIsPrivateAndRejectsSymlink(t *testing.T) {
	ctx := context.Background()
	repo := t.TempDir()
	store := NewFileStore(repo)
	if err := store.Append(ctx, models.LedgerEntry{
		Timestamp: time.Now(),
		Query:     "private",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	ledgerPath := filepath.Join(repo, ".neurofs", "ledger.jsonl")
	info, err := os.Lstat(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode = %v (%o), want regular 600", info.Mode(), info.Mode().Perm())
	}

	if err := os.Remove(ledgerPath); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	const sentinel = "outside must not change\n"
	if err := os.WriteFile(outside, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, ledgerPath); err != nil {
		t.Fatal(err)
	}

	if err := store.Append(ctx, models.LedgerEntry{Query: "unsafe"}); err == nil {
		t.Fatal("Append followed ledger symlink")
	}
	if _, err := store.Read(ctx, ""); err == nil {
		t.Fatal("Read followed ledger symlink")
	}
	if _, err := store.Search(ctx, "outside"); err == nil {
		t.Fatal("Search followed ledger symlink")
	}
	if _, err := store.Prune(ctx, time.Hour); err == nil {
		t.Fatal("Prune followed ledger symlink")
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sentinel {
		t.Fatalf("symlink target changed: %q", got)
	}
}

func TestMemStoreSearch(t *testing.T) {
	ms := NewMemStore()
	ctx := context.Background()
	m := New(ms)

	err := m.AppendEntry(ctx, models.LedgerEntry{
		Query: "find all nodes",
		Notes: "some memo",
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := m.SearchEntries(ctx, "find")
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("expected 1 result, got %d", len(res))
	}
	if res[0].Query != "find all nodes" {
		t.Errorf("query wrong: %q", res[0].Query)
	}
}

func TestExportEntries(t *testing.T) {
	tempDir := t.TempDir()
	fs := NewSqliteStore(tempDir)
	ctx := context.Background()
	m := New(fs)

	sessionID := "test-session-xyz"
	t.Setenv("NEUROFS_SESSION_ID", sessionID)

	err := m.AppendEntry(ctx, models.LedgerEntry{
		Query:   "implement something",
		Outcome: "success",
		Notes:   "working",
		Files:   []string{"main.go"},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = m.AppendEntry(ctx, models.LedgerEntry{
		Command: "go test",
		Outcome: "pass",
		Notes:   "all green",
	})
	if err != nil {
		t.Fatal(err)
	}

	timelineExport, err := m.ExportEntries(ctx, "", "session_timeline")
	if err != nil {
		t.Fatalf("session_timeline export failed: %v", err)
	}
	if !strings.Contains(timelineExport, "NEUROFS_SESSION.md") || !strings.Contains(timelineExport, "implement something") {
		t.Errorf("timeline export format invalid: %s", timelineExport)
	}

	agentsExport, err := m.ExportEntries(ctx, "", "agents")
	if err != nil {
		t.Fatalf("agents export failed: %v", err)
	}
	if !strings.Contains(agentsExport, "AGENTS.md") || !strings.Contains(agentsExport, "test-session-xyz") {
		t.Errorf("agents export format invalid: %s", agentsExport)
	}

	mdExport, err := m.ExportEntries(ctx, "", "markdown")
	if err != nil {
		t.Fatalf("markdown export failed: %v", err)
	}
	if !strings.Contains(mdExport, "Session Ledger Log") || !strings.Contains(mdExport, "go test") {
		t.Errorf("markdown export format invalid: %s", mdExport)
	}
}

func TestSlidingWindowExpiry(t *testing.T) {
	tempDir := t.TempDir()
	fs := NewSqliteStore(tempDir)
	ctx := context.Background()

	// Get initial session ID
	id1, err := fs.GetSessionID(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Move modification time back 7 hours (still valid)
	sessionFile := filepath.Join(tempDir, ".neurofs", "session.txt")
	past := time.Now().Add(-7 * time.Hour)
	err = os.Chtimes(sessionFile, past, past)
	if err != nil {
		t.Fatal(err)
	}

	// Append an entry (should touch session.txt and slide the window)
	err = fs.Append(ctx, models.LedgerEntry{Query: "ping"})
	if err != nil {
		t.Fatal(err)
	}

	// Check modification time. It should be reset to current time (or close to it)
	info, err := os.Stat(sessionFile)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) > 5*time.Second {
		t.Errorf("expected session.txt to be touched, but mtime was %v ago", time.Since(info.ModTime()))
	}

	// Session ID should remain the same (did not expire)
	id2, err := fs.GetSessionID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Errorf("expected session ID to persist after sliding touch; got %s vs %s", id1, id2)
	}
}

func TestPrune(t *testing.T) {
	ctx := context.Background()

	// 1. MemStore Prune
	ms := NewMemStore()
	err := ms.Append(ctx, models.LedgerEntry{
		Query:     "old query",
		Timestamp: time.Now().Add(-60 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = ms.Append(ctx, models.LedgerEntry{
		Query:     "new query",
		Timestamp: time.Now().Add(-1 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	count, err := ms.Prune(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected to prune 1 entry from memstore, got %d", count)
	}
	kept, err := ms.Read(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(kept) != 1 || kept[0].Query != "new query" {
		t.Errorf("expected to keep only 'new query', got: %+v", kept)
	}

	// 2. SqliteStore Prune
	tempDir := t.TempDir()
	sqlStore := NewSqliteStore(tempDir)

	// Touch last_prune_sqlite.txt to prevent background auto-prune during test appends
	pruneFileSql := filepath.Join(tempDir, ".neurofs", "last_prune_sqlite.txt")
	_ = os.MkdirAll(filepath.Dir(pruneFileSql), 0755)
	_ = os.WriteFile(pruneFileSql, []byte(time.Now().Format(time.RFC3339)), 0644)

	err = sqlStore.Append(ctx, models.LedgerEntry{
		Query:     "old query",
		Timestamp: time.Now().Add(-60 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = sqlStore.Append(ctx, models.LedgerEntry{
		Query:     "new query",
		Timestamp: time.Now().Add(-1 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	count, err = sqlStore.Prune(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected to prune 1 entry from sqlite, got %d", count)
	}
	keptSql, err := sqlStore.Read(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(keptSql) != 1 || keptSql[0].Query != "new query" {
		t.Errorf("expected to keep only 'new query' in sqlite, got: %+v", keptSql)
	}

	// 3. FileStore Prune
	tempDirFile := t.TempDir()
	fileStore := NewFileStore(tempDirFile)

	// Touch last_prune.txt to prevent background auto-prune during test appends
	pruneFileFile := filepath.Join(tempDirFile, ".neurofs", "last_prune.txt")
	_ = os.MkdirAll(filepath.Dir(pruneFileFile), 0755)
	_ = os.WriteFile(pruneFileFile, []byte(time.Now().Format(time.RFC3339)), 0644)

	err = fileStore.Append(ctx, models.LedgerEntry{
		Query:     "old query",
		Timestamp: time.Now().Add(-60 * 24 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	err = fileStore.Append(ctx, models.LedgerEntry{
		Query:     "new query",
		Timestamp: time.Now().Add(-1 * time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	count, err = fileStore.Prune(ctx, 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Errorf("expected to prune 1 entry from filestore, got %d", count)
	}
	keptFile, err := fileStore.Read(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(keptFile) != 1 || keptFile[0].Query != "new query" {
		t.Errorf("expected to keep only 'new query' in filestore, got: %+v", keptFile)
	}
	ledgerInfo, err := os.Lstat(filepath.Join(tempDirFile, ".neurofs", "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !ledgerInfo.Mode().IsRegular() || ledgerInfo.Mode().Perm() != 0o600 {
		t.Fatalf("pruned ledger mode = %v (%o), want regular 600", ledgerInfo.Mode(), ledgerInfo.Mode().Perm())
	}
}

func TestSqliteAutoPruneCompletesBeforeAppendReturns(t *testing.T) {
	repo := t.TempDir()
	store := NewSqliteStore(repo)
	ctx := context.Background()
	lockPath := filepath.Join(repo, ".neurofs", "last_prune_sqlite.lock")
	if err := os.MkdirAll(filepath.Dir(lockPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("orphaned"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * autoPruneLockStaleAfter)
	if err := os.Chtimes(lockPath, stale, stale); err != nil {
		t.Fatal(err)
	}

	if err := store.Append(ctx, models.LedgerEntry{
		SessionID: "old-session",
		Query:     "expired",
		Timestamp: time.Now().Add(-60 * 24 * time.Hour),
	}); err != nil {
		t.Fatalf("append expired entry: %v", err)
	}

	marker := filepath.Join(repo, ".neurofs", "last_prune_sqlite.txt")
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("auto-prune marker missing when Append returned: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("auto-prune lock was not released: %v", err)
	}
	entries, err := store.Read(ctx, "")
	if err != nil {
		t.Fatalf("read after auto-prune: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expired entry survived synchronous auto-prune: %+v", entries)
	}
}

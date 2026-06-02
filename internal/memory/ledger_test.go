package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/neuromfs/neuromfs/internal/models"
)

func TestGetSessionID(t *testing.T) {
	tempDir := t.TempDir()
	fs := NewSqliteStore(tempDir)
	ctx := context.Background()

	// 1. Env Var override
	os.Setenv("NEUROFS_SESSION_ID", "env-session-123")
	defer os.Unsetenv("NEUROFS_SESSION_ID")

	id, err := fs.GetSessionID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id != "env-session-123" {
		t.Errorf("expected env override 'env-session-123', got %q", id)
	}

	// Unset to test file-based
	os.Unsetenv("NEUROFS_SESSION_ID")

	// 2. Fresh session creation
	id1, err := fs.GetSessionID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(id1, "sess-") {
		t.Errorf("expected session prefix 'sess-', got %q", id1)
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
	sessionFile := filepath.Join(tempDir, ".neurofs", "session.txt")
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

	entries, err := fs.Read(ctx)
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
	os.Setenv("NEUROFS_SESSION_ID", sessionID)
	defer os.Unsetenv("NEUROFS_SESSION_ID")

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

	timelineExport, err := m.ExportEntries(ctx, "session_timeline")
	if err != nil {
		t.Fatalf("session_timeline export failed: %v", err)
	}
	if !strings.Contains(timelineExport, "NEUROFS_SESSION.md") || !strings.Contains(timelineExport, "implement something") {
		t.Errorf("timeline export format invalid: %s", timelineExport)
	}

	agentsExport, err := m.ExportEntries(ctx, "agents")
	if err != nil {
		t.Fatalf("agents export failed: %v", err)
	}
	if !strings.Contains(agentsExport, "AGENTS.md") || !strings.Contains(agentsExport, "test-session-xyz") {
		t.Errorf("agents export format invalid: %s", agentsExport)
	}

	mdExport, err := m.ExportEntries(ctx, "markdown")
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

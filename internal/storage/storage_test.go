package storage

import (
	"math"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Gere2/neurofs/internal/models"
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

func TestOpenRejectsSymlinkDatabasePath(t *testing.T) {
	root := t.TempDir()
	stateDir := filepath.Join(root, ".neurofs")
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.db")
	const sentinel = "external file must remain unchanged"
	if err := os.WriteFile(outside, []byte(sentinel), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(stateDir, "index.db")
	if err := os.Symlink(outside, dbPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if db, err := Open(dbPath); err == nil {
		_ = db.Close()
		t.Fatal("Open followed a database symlink")
	}
	got, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != sentinel {
		t.Fatalf("external target changed: %q", got)
	}
}

func TestOpenReadOnlyDoesNotCreateMissingDatabase(t *testing.T) {
	root := t.TempDir()
	dbPath := filepath.Join(root, "missing", "index.db")

	if db, err := OpenReadOnly(dbPath); err == nil {
		_ = db.Close()
		t.Fatal("OpenReadOnly created a missing database")
	}
	if _, err := os.Lstat(filepath.Dir(dbPath)); !os.IsNotExist(err) {
		t.Fatalf("OpenReadOnly created state for a missing index: %v", err)
	}
}

func TestOpenReadOnlyRejectsPendingWAL(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "index.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dbPath+"-wal", []byte("pending"), 0o600); err != nil {
		t.Fatal(err)
	}

	if readOnly, err := OpenReadOnly(dbPath); err == nil {
		_ = readOnly.Close()
		t.Fatal("OpenReadOnly accepted an index with a non-empty WAL")
	}
}

func TestOpenMigratesGenerationAndEmbeddingColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.db")
	db, err := Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`ALTER TABLE files DROP COLUMN mtime_ns`,
		`ALTER TABLE file_embeddings DROP COLUMN checksum`,
		`ALTER TABLE file_embeddings DROP COLUMN provider`,
		`ALTER TABLE file_embeddings DROP COLUMN model`,
		`ALTER TABLE chunks DROP COLUMN calls`,
	} {
		if _, err := db.db.Exec(stmt); err != nil {
			_ = db.Close()
			t.Fatalf("prepare legacy schema (%s): %v", stmt, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	migrated, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open legacy database: %v", err)
	}
	defer func() {
		if err := migrated.Close(); err != nil {
			t.Errorf("close migrated database: %v", err)
		}
	}()
	record := models.FileRecord{
		Path: "/repo/migrated.go", RelPath: "migrated.go", Lang: models.LangGo,
		ModTimeUnixNano: 123, Checksum: "sum", IndexedAt: time.Now(),
	}
	if err := migrated.UpsertFileAndChunks(record, []models.Chunk{{
		ChunkID: "chunk", FilePath: record.Path, ContentHash: "hash", Calls: []string{"Call"},
	}}); err != nil {
		t.Fatalf("write migrated columns: %v", err)
	}
	if err := migrated.SaveEmbeddingWithMetadata(
		record.Path, []float32{1}, record.Checksum, "mock", "mock-lcg",
	); err != nil {
		t.Fatalf("write migrated embedding columns: %v", err)
	}
}

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
			defer func() { _ = db.Close() }()
			for i := 0; i < opsPerWriter; i++ {
				p := filepath.Join("/fake", string(rune('a'+id)), string(rune('a'+i%26)))
				if err := db.UpsertFile(models.FileRecord{
					Path:      p,
					RelPath:   p,
					Lang:      models.LangGo,
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

func TestFileOperations(t *testing.T) {
	db := newTempDB(t)

	now := time.Now().Truncate(time.Second) // SQLite time precision
	f1 := models.FileRecord{
		Path:      "/repo/main.go",
		RelPath:   "main.go",
		Lang:      models.LangGo,
		Size:      1024,
		Lines:     100,
		Symbols:   []models.Symbol{{Name: "main", Kind: "func", Line: 5}},
		Imports:   []fmtString{"fmt"},
		Checksum:  "abc",
		IndexedAt: now,
	}

	// 1. Test UpsertFile (insert)
	if err := db.UpsertFile(f1); err != nil {
		t.Fatalf("UpsertFile insert: %v", err)
	}

	// 2. Test GetFileByRelPath (exist)
	got, err := db.GetFileByRelPath("main.go")
	if err != nil {
		t.Fatalf("GetFileByRelPath: %v", err)
	}
	if got.Path != f1.Path || got.Size != f1.Size || len(got.Symbols) != 1 || got.Symbols[0].Name != "main" {
		t.Errorf("GetFileByRelPath returned incorrect record: %+v", got)
	}

	// Test GetFileByRelPath (non-existent)
	_, err = db.GetFileByRelPath("missing.go")
	if err == nil {
		t.Error("expected error for non-existent file, got nil")
	}

	// 3. Test UpsertFile (conflict/update)
	f1.Size = 2048
	f1.Checksum = "def"
	if err := db.UpsertFile(f1); err != nil {
		t.Fatalf("UpsertFile update: %v", err)
	}

	got, err = db.GetFileByRelPath("main.go")
	if err != nil {
		t.Fatalf("GetFileByRelPath: %v", err)
	}
	if got.Size != 2048 || got.Checksum != "def" {
		t.Errorf("expected updated size=2048, checksum=def; got size=%d, checksum=%s", got.Size, got.Checksum)
	}

	// 4. Test FileCount
	count, err := db.FileCount()
	if err != nil {
		t.Fatalf("FileCount: %v", err)
	}
	if count != 1 {
		t.Errorf("expected file count 1, got %d", count)
	}

	// Add second file
	f2 := models.FileRecord{
		Path:      "/repo/utils.py",
		RelPath:   "utils.py",
		Lang:      models.LangPython,
		Size:      500,
		Lines:     50,
		Checksum:  "xyz",
		IndexedAt: now.Add(time.Hour),
	}
	if err := db.UpsertFile(f2); err != nil {
		t.Fatalf("UpsertFile f2: %v", err)
	}

	count, _ = db.FileCount()
	if count != 2 {
		t.Errorf("expected file count 2, got %d", count)
	}

	// 5. Test LangBreakdown
	breakdown, err := db.LangBreakdown()
	if err != nil {
		t.Fatalf("LangBreakdown: %v", err)
	}
	if breakdown[models.LangGo] != 1 || breakdown[models.LangPython] != 1 {
		t.Errorf("incorrect language breakdown: %+v", breakdown)
	}

	// 6. Test LastIndexedAt
	last, err := db.LastIndexedAt()
	if err != nil {
		t.Fatalf("LastIndexedAt: %v", err)
	}
	expectedTime := now.Add(time.Hour).UTC()
	if !last.Equal(expectedTime) {
		t.Errorf("expected LastIndexedAt %v, got %v", expectedTime, last)
	}

	// 7. Test TotalBytes
	total, err := db.TotalBytes()
	if err != nil {
		t.Fatalf("TotalBytes: %v", err)
	}
	if total != 2548 { // 2048 + 500
		t.Errorf("expected total bytes 2548, got %d", total)
	}

	// 8. Test AllFiles
	all, err := db.AllFiles()
	if err != nil {
		t.Fatalf("AllFiles: %v", err)
	}
	if len(all) != 2 {
		t.Errorf("expected 2 files from AllFiles, got %d", len(all))
	}

	// 9. Test DeleteRemovedFiles
	// Mock existing files on disk: main.go remains, utils.py is gone.
	existing := map[string]bool{
		"/repo/main.go": true,
	}
	deleted, err := db.DeleteRemovedFiles(existing)
	if err != nil {
		t.Fatalf("DeleteRemovedFiles: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 file deleted, got %d", deleted)
	}

	count, _ = db.FileCount()
	if count != 1 {
		t.Errorf("expected remaining count 1, got %d", count)
	}

	// 10. Test DeleteFile
	if err := db.DeleteFile("/repo/main.go"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}
	count, _ = db.FileCount()
	if count != 0 {
		t.Errorf("expected count 0 after DeleteFile, got %d", count)
	}
}

func TestMetadataOperations(t *testing.T) {
	db := newTempDB(t)

	// Set & Get non-existent
	val, ok, err := db.GetMeta("non-existent")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if ok || val != "" {
		t.Errorf("expected (empty, false), got (%q, %t)", val, ok)
	}

	// Set and override
	if err := db.SetMeta("version", "1.0.0"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	val, ok, err = db.GetMeta("version")
	if err != nil || !ok || val != "1.0.0" {
		t.Errorf("GetMeta version: got (%q, %t), err: %v", val, ok, err)
	}

	if err := db.SetMeta("version", "1.1.0"); err != nil {
		t.Fatalf("SetMeta override: %v", err)
	}
	val, _, _ = db.GetMeta("version")
	if val != "1.1.0" {
		t.Errorf("expected updated metadata 1.1.0, got %q", val)
	}
}

func TestProxyLogOperations(t *testing.T) {
	db := newTempDB(t)

	now := time.Now().Truncate(time.Second)
	err := db.InsertProxyLog(now, "gpt-4", "hello", 100, 150, 50, 0.0015)
	if err != nil {
		t.Fatalf("InsertProxyLog: %v", err)
	}
	err = db.InsertProxyLog(now.Add(time.Second), "gpt-4", "world", 200, 220, 80, 0.0024)
	if err != nil {
		t.Fatalf("InsertProxyLog 2: %v", err)
	}

	logs, err := db.GetProxyLogs(1)
	if err != nil {
		t.Fatalf("GetProxyLogs: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("expected 1 log, got %d", len(logs))
	}
	if logs[0].Query != "world" {
		t.Errorf("expected logs ordered by desc time, got query %q", logs[0].Query)
	}

	count, saved, usd, err := db.GetProxySummary()
	if err != nil {
		t.Fatalf("GetProxySummary: %v", err)
	}
	if count != 2 {
		t.Errorf("expected count 2, got %d", count)
	}
	if saved != 130 {
		t.Errorf("expected saved 130, got %d", saved)
	}
	if math.Abs(usd-0.0039) > 1e-9 {
		t.Errorf("expected usd 0.0039, got %f", usd)
	}
}

func TestEmbeddingOperations(t *testing.T) {
	db := newTempDB(t)

	// Since foreign key refers to files(path) cascade delete,
	// insert mock files first
	f := models.FileRecord{Path: "/repo/a.go", RelPath: "a.go", Lang: models.LangGo, Checksum: "x", IndexedAt: time.Now()}
	if err := db.UpsertFile(f); err != nil {
		t.Fatalf("insert file: %v", err)
	}

	emb := []float32{0.1, -0.2, 0.5}
	if err := db.SaveEmbedding("/repo/a.go", emb); err != nil {
		t.Fatalf("SaveEmbedding: %v", err)
	}

	got, found, err := db.GetEmbedding("/repo/a.go")
	if err != nil {
		t.Fatalf("GetEmbedding: %v", err)
	}
	if !found {
		t.Fatal("embedding not found")
	}
	if len(got) != 3 || got[0] != 0.1 || got[1] != -0.2 {
		t.Errorf("retrieved incorrect embedding vector: %v", got)
	}

	_, found, _ = db.GetEmbedding("/repo/missing.go")
	if found {
		t.Error("expected false for missing embedding path")
	}

	all, err := db.AllEmbeddings()
	if err != nil {
		t.Fatalf("AllEmbeddings: %v", err)
	}
	if len(all) != 1 || len(all["/repo/a.go"]) != 3 {
		t.Errorf("AllEmbeddings returned incorrect map: %v", all)
	}
}

func TestRelationOperations(t *testing.T) {
	db := newTempDB(t)

	// Insert files for referential integrity
	f1 := models.FileRecord{Path: "/repo/a.go", RelPath: "a.go", Lang: models.LangGo, Checksum: "a", IndexedAt: time.Now()}
	f2 := models.FileRecord{Path: "/repo/b.go", RelPath: "b.go", Lang: models.LangGo, Checksum: "b", IndexedAt: time.Now()}
	if err := db.UpsertFile(f1); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFile(f2); err != nil {
		t.Fatal(err)
	}

	rels := []models.FileRelation{
		{SourcePath: "/repo/a.go", TargetPath: "/repo/b.go", RelType: "import"},
	}

	if err := db.UpdateRelations(rels); err != nil {
		t.Fatalf("UpdateRelations: %v", err)
	}

	all, err := db.AllRelations()
	if err != nil {
		t.Fatalf("AllRelations: %v", err)
	}
	if len(all) != 1 || all[0].SourcePath != "/repo/a.go" {
		t.Errorf("AllRelations output incorrect: %+v", all)
	}

	srcRels, err := db.GetRelationsForSource("/repo/a.go")
	if err != nil || len(srcRels) != 1 {
		t.Errorf("GetRelationsForSource failed: %v, rels: %+v", err, srcRels)
	}

	tgtRels, err := db.GetRelationsForTarget("/repo/b.go")
	if err != nil || len(tgtRels) != 1 {
		t.Errorf("GetRelationsForTarget failed: %v, rels: %+v", err, tgtRels)
	}
}

func TestChunkOperations(t *testing.T) {
	db := newTempDB(t)

	f := models.FileRecord{Path: "/repo/a.go", RelPath: "a.go", Lang: models.LangGo, Checksum: "a", IndexedAt: time.Now()}
	if err := db.UpsertFile(f); err != nil {
		t.Fatal(err)
	}

	chunks := []models.Chunk{
		{
			ChunkID:       "chunk1",
			FilePath:      "/repo/a.go",
			ParentID:      "",
			Kind:          "func",
			Symbol:        "MyFunc",
			StartLine:     1,
			EndLine:       10,
			ContentHash:   "hash1",
			ASTHash:       "ast1",
			Calls:         []string{"helper", "svc.Run"},
			TokenEstimate: 30,
		},
		{
			ChunkID:       "chunk2",
			FilePath:      "/repo/a.go",
			ParentID:      "",
			Kind:          "func",
			Symbol:        "OtherFunc",
			StartLine:     11,
			EndLine:       20,
			ContentHash:   "hash2",
			ASTHash:       "ast2",
			TokenEstimate: 40,
		},
	}

	if err := db.UpdateChunks("/repo/a.go", chunks); err != nil {
		t.Fatalf("UpdateChunks: %v", err)
	}

	all, err := db.AllChunks()
	if err != nil {
		t.Fatalf("AllChunks: %v", err)
	}
	if len(all) != 2 || all[0].ChunkID != "chunk1" {
		t.Errorf("AllChunks returned unexpected: %+v", all)
	}
	if len(all[0].Calls) != 2 || all[0].Calls[0] != "helper" {
		t.Errorf("Calls did not roundtrip: %+v", all[0].Calls)
	}

	gotChunks, err := db.GetChunksForFile("/repo/a.go")
	if err != nil || len(gotChunks) != 2 {
		t.Errorf("GetChunksForFile failed: %v, got: %+v", err, gotChunks)
	}

	// Test SearchChunks with various search filters
	// 1. By FilePath
	res, _ := db.SearchChunks(ChunkSearchOptions{FilePath: "/repo/a.go"})
	if len(res) != 2 {
		t.Errorf("expected 2 chunks, got %d", len(res))
	}

	// 2. By Symbol substring (case insensitive)
	res, _ = db.SearchChunks(ChunkSearchOptions{Symbol: "myfu"})
	if len(res) != 1 || res[0].Symbol != "MyFunc" {
		t.Errorf("expected MyFunc, got %+v", res)
	}

	// 3. By ContentHash
	res, _ = db.SearchChunks(ChunkSearchOptions{ContentHash: "hash2"})
	if len(res) != 1 || res[0].ChunkID != "chunk2" {
		t.Errorf("expected chunk2, got %+v", res)
	}

	// 4. Limit
	res, _ = db.SearchChunks(ChunkSearchOptions{Limit: 1})
	if len(res) != 1 {
		t.Errorf("expected limit of 1 chunk, got %d", len(res))
	}

	// Test Chunk Embeddings
	emb := []float32{0.5, 0.6, 0.7}
	if err := db.SaveChunkEmbedding("hash1", emb, "openai", "text-embedding-3-small"); err != nil {
		t.Fatalf("SaveChunkEmbedding: %v", err)
	}

	gotEmb, found, err := db.GetChunkEmbedding("hash1")
	if err != nil || !found || len(gotEmb) != 3 {
		t.Errorf("GetChunkEmbedding failed: %v, found: %t, vec: %v", err, found, gotEmb)
	}

	allEmbs, err := db.AllChunkEmbeddings()
	if err != nil || len(allEmbs) != 1 || len(allEmbs["hash1"]) != 3 {
		t.Errorf("AllChunkEmbeddings failed: %v, got: %v", err, allEmbs)
	}
}

func TestChunkEmbeddingsPrunedOnChunkUpdate(t *testing.T) {
	db := newTempDB(t)

	f := models.FileRecord{Path: "/repo/a.go", RelPath: "a.go", Lang: models.LangGo, Checksum: "a", IndexedAt: time.Now()}
	if err := db.UpsertFile(f); err != nil {
		t.Fatal(err)
	}

	if err := db.UpdateChunks("/repo/a.go", []models.Chunk{
		{ChunkID: "one", FilePath: "/repo/a.go", ContentHash: "hash1"},
		{ChunkID: "two", FilePath: "/repo/a.go", ContentHash: "hash2"},
	}); err != nil {
		t.Fatalf("initial UpdateChunks: %v", err)
	}
	for _, hash := range []string{"hash1", "hash2", "hash3", "orphan"} {
		if err := db.SaveChunkEmbedding(hash, []float32{1.0}, "mock", "mock-lcg"); err != nil {
			t.Fatalf("SaveChunkEmbedding %s: %v", hash, err)
		}
	}

	if err := db.UpdateChunks("/repo/a.go", []models.Chunk{
		{ChunkID: "two", FilePath: "/repo/a.go", ContentHash: "hash2"},
		{ChunkID: "three", FilePath: "/repo/a.go", ContentHash: "hash3"},
	}); err != nil {
		t.Fatalf("updated UpdateChunks: %v", err)
	}

	embs, err := db.AllChunkEmbeddings()
	if err != nil {
		t.Fatalf("AllChunkEmbeddings: %v", err)
	}
	if len(embs) != 2 {
		t.Fatalf("expected 2 referenced embeddings, got %d: %+v", len(embs), embs)
	}
	for _, hash := range []string{"hash2", "hash3"} {
		if _, ok := embs[hash]; !ok {
			t.Fatalf("expected referenced embedding %s to remain: %+v", hash, embs)
		}
	}
	for _, hash := range []string{"hash1", "orphan"} {
		if _, ok := embs[hash]; ok {
			t.Fatalf("expected unreferenced embedding %s to be pruned: %+v", hash, embs)
		}
	}
}

func TestDeleteFilePrunesUnreferencedChunkEmbeddings(t *testing.T) {
	db := newTempDB(t)

	fileA := models.FileRecord{Path: "/repo/a.go", RelPath: "a.go", Lang: models.LangGo, Checksum: "a", IndexedAt: time.Now()}
	fileB := models.FileRecord{Path: "/repo/b.go", RelPath: "b.go", Lang: models.LangGo, Checksum: "b", IndexedAt: time.Now()}
	if err := db.UpsertFile(fileA); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertFile(fileB); err != nil {
		t.Fatal(err)
	}

	if err := db.UpdateChunks("/repo/a.go", []models.Chunk{
		{ChunkID: "a-only", FilePath: "/repo/a.go", ContentHash: "a-only"},
		{ChunkID: "shared", FilePath: "/repo/a.go", ContentHash: "shared"},
	}); err != nil {
		t.Fatalf("UpdateChunks fileA: %v", err)
	}
	if err := db.UpdateChunks("/repo/b.go", []models.Chunk{
		{ChunkID: "shared", FilePath: "/repo/b.go", ContentHash: "shared"},
	}); err != nil {
		t.Fatalf("UpdateChunks fileB: %v", err)
	}
	for _, hash := range []string{"a-only", "shared"} {
		if err := db.SaveChunkEmbedding(hash, []float32{1.0}, "mock", "mock-lcg"); err != nil {
			t.Fatalf("SaveChunkEmbedding %s: %v", hash, err)
		}
	}
	if err := db.UpdateRelations([]models.FileRelation{
		{SourcePath: "/repo/a.go", TargetPath: "/repo/b.go", RelType: "import"},
		{SourcePath: "/repo/b.go", TargetPath: "/repo/a.go", RelType: "import"},
	}); err != nil {
		t.Fatalf("UpdateRelations: %v", err)
	}

	if err := db.DeleteFile("/repo/a.go"); err != nil {
		t.Fatalf("DeleteFile: %v", err)
	}

	embs, err := db.AllChunkEmbeddings()
	if err != nil {
		t.Fatalf("AllChunkEmbeddings: %v", err)
	}
	if _, ok := embs["a-only"]; ok {
		t.Fatalf("expected deleted file's private embedding to be pruned: %+v", embs)
	}
	if _, ok := embs["shared"]; !ok || len(embs) != 1 {
		t.Fatalf("expected shared embedding to remain: %+v", embs)
	}

	rels, err := db.AllRelations()
	if err != nil {
		t.Fatalf("AllRelations: %v", err)
	}
	if len(rels) != 0 {
		t.Fatalf("expected relations touching deleted file to be removed: %+v", rels)
	}
}

func TestUpsertFileAndChunksPublishesOneGeneration(t *testing.T) {
	db := newTempDB(t)
	path := filepath.Join("/repo", "generation.go")
	oldRecord := models.FileRecord{
		Path: path, RelPath: "generation.go", Lang: models.LangGo,
		Size: 10, ModTimeUnixNano: 100, Checksum: "old-checksum", IndexedAt: time.Now(),
	}
	oldChunks := []models.Chunk{{
		ChunkID: "old", FilePath: path, ContentHash: "old-hash", Symbol: "Old",
	}}
	if err := db.UpsertFileAndChunks(oldRecord, oldChunks); err != nil {
		t.Fatalf("initial generation: %v", err)
	}
	if err := db.SaveEmbeddingWithMetadata(
		path, []float32{1}, oldRecord.Checksum, "mock", "mock-lcg",
	); err != nil {
		t.Fatalf("save old file embedding: %v", err)
	}
	if err := db.SaveChunkEmbedding("old-hash", []float32{1}, "mock", "mock-lcg"); err != nil {
		t.Fatalf("save old chunk embedding: %v", err)
	}

	newRecord := oldRecord
	newRecord.Size = 20
	newRecord.ModTimeUnixNano = 200
	newRecord.Checksum = "new-checksum"
	newRecord.IndexedAt = time.Now().Add(time.Second)
	newChunks := []models.Chunk{{
		ChunkID: "new", FilePath: path, ContentHash: "new-hash", Symbol: "New",
	}}
	if err := db.UpsertFileAndChunks(newRecord, newChunks); err != nil {
		t.Fatalf("replace generation: %v", err)
	}

	got, err := db.GetFileByRelPath("generation.go")
	if err != nil {
		t.Fatalf("get new file record: %v", err)
	}
	if got.Checksum != newRecord.Checksum || got.ModTimeUnixNano != newRecord.ModTimeUnixNano {
		t.Fatalf("file generation = %+v, want checksum/mtime from new generation", got)
	}
	chunks, err := db.GetChunksForFile(path)
	if err != nil {
		t.Fatalf("get new chunks: %v", err)
	}
	if len(chunks) != 1 || chunks[0].ChunkID != "new" || chunks[0].ContentHash != "new-hash" {
		t.Fatalf("chunks = %+v, want only new generation", chunks)
	}
	if _, found, err := db.GetEmbedding(path); err != nil || found {
		t.Fatalf("stale file embedding found=%t err=%v", found, err)
	}

	if err := db.PruneUnreferencedChunkEmbeddings(); err != nil {
		t.Fatalf("prune old chunk embeddings: %v", err)
	}
	if _, found, err := db.GetChunkEmbedding("old-hash"); err != nil || found {
		t.Fatalf("stale chunk embedding found=%t err=%v", found, err)
	}
}

func TestEmbeddingReadsFilterChecksumAndProvider(t *testing.T) {
	db := newTempDB(t)
	records := []models.FileRecord{
		{Path: "/repo/match.go", RelPath: "match.go", Lang: models.LangGo, Checksum: "match", IndexedAt: time.Now()},
		{Path: "/repo/provider.go", RelPath: "provider.go", Lang: models.LangGo, Checksum: "provider", IndexedAt: time.Now()},
		{Path: "/repo/stale.go", RelPath: "stale.go", Lang: models.LangGo, Checksum: "current", IndexedAt: time.Now()},
	}
	for _, record := range records {
		if err := db.UpsertFile(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SaveEmbeddingWithMetadata("/repo/match.go", []float32{1}, "match", "mock", "mock-lcg"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEmbeddingWithMetadata("/repo/provider.go", []float32{2}, "provider", "openai", "other"); err != nil {
		t.Fatal(err)
	}
	if err := db.SaveEmbeddingWithMetadata("/repo/stale.go", []float32{3}, "old", "mock", "mock-lcg"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetMeta("embedding_provider", "mock:mock-lcg"); err != nil {
		t.Fatal(err)
	}

	embeddings, err := db.AllEmbeddings()
	if err != nil {
		t.Fatalf("AllEmbeddings: %v", err)
	}
	if len(embeddings) != 1 || len(embeddings["/repo/match.go"]) != 1 {
		t.Fatalf("filtered embeddings = %+v, want only exact checksum/provider match", embeddings)
	}
}

func TestDeletePathTreeDoesNotDeletePrefixSibling(t *testing.T) {
	db := newTempDB(t)
	paths := []string{
		filepath.Join("/repo", "pkg", "a.go"),
		filepath.Join("/repo", "pkg", "nested", "b.go"),
		filepath.Join("/repo", "pkg2", "keep.go"),
	}
	for _, path := range paths {
		if err := db.UpsertFile(models.FileRecord{
			Path: path, RelPath: path, Lang: models.LangGo,
			Checksum: path, IndexedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
	}

	deleted, err := db.DeletePathTree(filepath.Join("/repo", "pkg"))
	if err != nil {
		t.Fatalf("DeletePathTree: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("deleted = %d, want 2 descendants", deleted)
	}
	files, err := db.AllFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != filepath.Join("/repo", "pkg2", "keep.go") {
		t.Fatalf("remaining files = %+v, want prefix sibling only", files)
	}
}

func TestClearIndex(t *testing.T) {
	db := newTempDB(t)

	f := models.FileRecord{Path: "/repo/a.go", RelPath: "a.go", Lang: models.LangGo, Checksum: "a", IndexedAt: time.Now()}
	_ = db.UpsertFile(f)
	_ = db.SetMeta("test", "value")

	if err := db.ClearIndex(); err != nil {
		t.Fatalf("ClearIndex: %v", err)
	}

	count, _ := db.FileCount()
	if count != 0 {
		t.Errorf("expected file count 0 after clear, got %d", count)
	}

	_, ok, _ := db.GetMeta("test")
	if ok {
		t.Error("expected metadata to be truncated")
	}
}

func TestCascadeDeletes(t *testing.T) {
	db := newTempDB(t)

	f := models.FileRecord{Path: "/repo/a.go", RelPath: "a.go", Lang: models.LangGo, Checksum: "a", IndexedAt: time.Now()}
	if err := db.UpsertFile(f); err != nil {
		t.Fatal(err)
	}

	// Add child records referencing "/repo/a.go"
	_ = db.SaveEmbedding("/repo/a.go", []float32{1.0, 2.0})
	_ = db.UpdateRelations([]models.FileRelation{{SourcePath: "/repo/a.go", TargetPath: "/repo/a.go", RelType: "self"}})
	_ = db.UpdateChunks("/repo/a.go", []models.Chunk{{ChunkID: "c1", FilePath: "/repo/a.go", ContentHash: "ch1"}})

	// Assert records are present
	embs, _ := db.AllEmbeddings()
	rels, _ := db.AllRelations()
	chunks, _ := db.AllChunks()
	if len(embs) != 1 || len(rels) != 1 || len(chunks) != 1 {
		t.Fatalf("expected child records before delete: embs=%d, rels=%d, chunks=%d", len(embs), len(rels), len(chunks))
	}

	// Delete file
	if err := db.DeleteFile("/repo/a.go"); err != nil {
		t.Fatalf("delete file: %v", err)
	}

	// Assert child records were cascade-deleted!
	embs, _ = db.AllEmbeddings()
	rels, _ = db.AllRelations()
	chunks, _ = db.AllChunks()
	if len(embs) != 0 || len(rels) != 0 || len(chunks) != 0 {
		t.Errorf("expected child records cascade deleted, remaining: embs=%d, rels=%d, chunks=%d", len(embs), len(rels), len(chunks))
	}
}

func TestDBSize(t *testing.T) {
	db := newTempDB(t)
	size, err := db.DBSize()
	if err != nil {
		t.Fatalf("DBSize error: %v", err)
	}
	if size <= 0 {
		t.Errorf("expected database size > 0, got %d", size)
	}
}

type fmtString = string // help imports/mock compile if models uses it

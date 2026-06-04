package memory

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/neuromfs/neuromfs/internal/models"
)

// FileStore implements the Store interface using local files.
type FileStore struct {
	repoRoot string
	mu       sync.RWMutex // guards files against concurrent processes within the same server instance
}

// NewFileStore constructs a FileStore rooted at the repository.
func NewFileStore(repoRoot string) *FileStore {
	return &FileStore{repoRoot: repoRoot}
}

// GetSessionID resolves the current active session ID.
func (fs *FileStore) GetSessionID(ctx context.Context) (string, error) {
	if envID := os.Getenv("NEUROFS_SESSION_ID"); envID != "" {
		return strings.TrimSpace(envID), nil
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	sessionFile := filepath.Join(fs.repoRoot, ".neurofs", "session.txt")
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
func (fs *FileStore) SaveSessionID(ctx context.Context, id string) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	sessionFile := filepath.Join(fs.repoRoot, ".neurofs", "session.txt")
	_ = os.MkdirAll(filepath.Dir(sessionFile), 0755)
	return os.WriteFile(sessionFile, []byte(id), 0644)
}

// Append logs a models.LedgerEntry to the local .neurofs/ledger.jsonl.
func (fs *FileStore) Append(ctx context.Context, entry models.LedgerEntry) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal entry: %w", err)
	}

	ledgerDir := filepath.Join(fs.repoRoot, ".neurofs")
	if err := os.MkdirAll(ledgerDir, 0755); err != nil {
		return fmt.Errorf("create ledger dir: %w", err)
	}

	// Reset sliding session window by touching session.txt
	sessionFile := filepath.Join(ledgerDir, "session.txt")
	if _, err := os.Stat(sessionFile); err == nil {
		now := time.Now()
		_ = os.Chtimes(sessionFile, now, now)
	}

	ledgerPath := filepath.Join(ledgerDir, "ledger.jsonl")
	f, err := os.OpenFile(ledgerPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open ledger file: %w", err)
	}
	defer f.Close()

	if _, err := f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write ledger entry: %w", err)
	}

	fs.checkAutoPrune(ctx)
	return nil
}

// Read parses entries from .neurofs/ledger.jsonl, optionally filtered by sessionID.
func (fs *FileStore) Read(ctx context.Context, sessionID string) ([]models.LedgerEntry, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	ledgerPath := filepath.Join(fs.repoRoot, ".neurofs", "ledger.jsonl")
	f, err := os.Open(ledgerPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	defer f.Close()

	var entries []models.LedgerEntry
	reader := bufio.NewReader(f)
	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && len(lineBytes) == 0 {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("read ledger line: %w", err)
		}

		line := strings.TrimSpace(string(lineBytes))
		if line == "" {
			if err == io.EOF {
				break
			}
			continue
		}

		// Fast pre-filter for session ID if requested
		if sessionID != "" && !strings.Contains(line, sessionID) {
			if err == io.EOF {
				break
			}
			continue
		}

		var entry models.LedgerEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			if err == io.EOF {
				break
			}
			continue // Skip corrupted logs to maintain resiliency
		}

		if sessionID == "" || entry.SessionID == sessionID {
			entries = append(entries, entry)
		}
		if err == io.EOF {
			break
		}
	}
	return entries, nil
}

// Search filters ledger entries containing term (case-insensitive) using a raw-line pre-filter.
func (fs *FileStore) Search(ctx context.Context, term string) ([]models.LedgerEntry, error) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()

	ledgerPath := filepath.Join(fs.repoRoot, ".neurofs", "ledger.jsonl")
	f, err := os.Open(ledgerPath)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open ledger: %w", err)
	}
	defer f.Close()

	term = strings.ToLower(strings.TrimSpace(term))

	var results []models.LedgerEntry
	reader := bufio.NewReader(f)
	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && len(lineBytes) == 0 {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("read ledger line: %w", err)
		}

		line := strings.TrimSpace(string(lineBytes))
		if line == "" {
			if err == io.EOF {
				break
			}
			continue
		}

		// Performance Optimization: Check if raw line contains the lowercase search term 
		// before invoking expensive JSON unmarshaling.
		if term != "" && !strings.Contains(strings.ToLower(line), term) {
			if err == io.EOF {
				break
			}
			continue
		}

		var entry models.LedgerEntry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			if err == io.EOF {
				break
			}
			continue // Skip corrupted logs
		}

		// Double check strictly on fields to avoid false positives from JSON structure formatting
		if term == "" || matchEntry(entry, term) {
			results = append(results, entry)
		}

		if err == io.EOF {
			break
		}
	}
	return results, nil
}

// Prune removes entries older than olderThan from the JSONL file.
func (fs *FileStore) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	ledgerPath := filepath.Join(fs.repoRoot, ".neurofs", "ledger.jsonl")
	f, err := os.Open(ledgerPath)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("open ledger for pruning: %w", err)
	}

	cutoff := time.Now().Add(-olderThan)
	var kept []models.LedgerEntry
	var count int64

	reader := bufio.NewReader(f)
	for {
		lineBytes, err := reader.ReadBytes('\n')
		if err != nil && len(lineBytes) == 0 {
			break
		}
		line := strings.TrimSpace(string(lineBytes))
		if line == "" {
			continue
		}
		var entry models.LedgerEntry
		if err := json.Unmarshal([]byte(line), &entry); err == nil {
			if entry.Timestamp.Before(cutoff) {
				count++
			} else {
				kept = append(kept, entry)
			}
		}
	}
	f.Close()

	if count == 0 {
		return 0, nil
	}

	// Rewrite ledger.jsonl atomically with kept entries
	tmpPath := ledgerPath + ".tmp"
	tmpFile, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0644)
	if err != nil {
		return 0, fmt.Errorf("create prune temp file: %w", err)
	}
	defer tmpFile.Close()

	for _, entry := range kept {
		data, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		if _, err := tmpFile.Write(append(data, '\n')); err != nil {
			return 0, fmt.Errorf("write kept entry: %w", err)
		}
	}
	tmpFile.Close()

	if err := os.Rename(tmpPath, ledgerPath); err != nil {
		return 0, fmt.Errorf("rename pruned ledger: %w", err)
	}
	return count, nil
}

// checkAutoPrune performs a non-blocking background check for pruning logs older than 30 days.
func (fs *FileStore) checkAutoPrune(ctx context.Context) {
	if flag.Lookup("test.v") != nil {
		return
	}
	pruneFile := filepath.Join(fs.repoRoot, ".neurofs", "last_prune.txt")
	if info, err := os.Stat(pruneFile); err == nil {
		if time.Since(info.ModTime()) < 24*time.Hour {
			return
		}
	}

	go func() {
		_, _ = fs.Prune(context.Background(), 30*24*time.Hour)
		_ = os.MkdirAll(filepath.Dir(pruneFile), 0755)
		_ = os.WriteFile(pruneFile, []byte(time.Now().Format(time.RFC3339)), 0644)
	}()
}

func matchEntry(entry models.LedgerEntry, term string) bool {
	if strings.Contains(strings.ToLower(entry.Query), term) ||
		strings.Contains(strings.ToLower(entry.Command), term) ||
		strings.Contains(strings.ToLower(entry.Outcome), term) ||
		strings.Contains(strings.ToLower(entry.Notes), term) ||
		strings.Contains(strings.ToLower(entry.SessionID), term) ||
		strings.Contains(strings.ToLower(entry.BundleHash), term) {
		return true
	}
	for _, file := range entry.Files {
		if strings.Contains(strings.ToLower(file), term) {
			return true
		}
	}
	return false
}

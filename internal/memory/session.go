package memory

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Gere2/neurofs/internal/atomicfile"
)

const maxSessionIDBytes = 256

func sessionIDFromEnv() (string, bool, error) {
	raw, configured := os.LookupEnv("NEUROFS_SESSION_ID")
	if !configured {
		return "", false, nil
	}
	id, err := normalizeSessionID(raw)
	if err != nil {
		return "", true, fmt.Errorf("invalid NEUROFS_SESSION_ID: %w", err)
	}
	return id, true, nil
}

func normalizeSessionID(raw string) (string, error) {
	if strings.ContainsAny(raw, "\r\n") {
		return "", fmt.Errorf("session ID must be one line")
	}
	id := strings.TrimSpace(raw)
	if id == "" {
		return "", fmt.Errorf("session ID must not be empty")
	}
	if len(id) > maxSessionIDBytes {
		return "", fmt.Errorf("session ID exceeds %d bytes", maxSessionIDBytes)
	}
	return id, nil
}

func newSessionID() (string, error) {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}
	return fmt.Sprintf("sess-%s", hex.EncodeToString(bytes)), nil
}

// loadFreshSessionID returns (id, true, nil) for a valid fresh regular file,
// ("", false, nil) for a missing or expired file, and an error for unsafe or
// malformed state.
func loadFreshSessionID(path string) (string, bool, error) {
	before, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("inspect session file: %w", err)
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return "", false, fmt.Errorf("session file must be a regular non-symlink file")
	}
	if time.Since(before.ModTime()) >= sessionDuration {
		return "", false, nil
	}
	if before.Size() > maxSessionIDBytes {
		return "", false, fmt.Errorf("session file exceeds %d bytes", maxSessionIDBytes)
	}

	file, err := os.Open(path)
	if err != nil {
		return "", false, fmt.Errorf("open session file: %w", err)
	}
	defer closeFile(file)
	opened, err := file.Stat()
	if err != nil {
		return "", false, fmt.Errorf("stat opened session file: %w", err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return "", false, fmt.Errorf("session file changed before read")
	}

	data, err := io.ReadAll(io.LimitReader(file, maxSessionIDBytes+1))
	if err != nil {
		return "", false, fmt.Errorf("read session file: %w", err)
	}
	if len(data) > maxSessionIDBytes {
		return "", false, fmt.Errorf("session file exceeds %d bytes", maxSessionIDBytes)
	}
	after, err := os.Lstat(path)
	if err != nil || after.Mode()&os.ModeSymlink != 0 ||
		!after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return "", false, fmt.Errorf("session file changed while reading")
	}
	id, err := normalizeSessionID(string(data))
	if err != nil {
		return "", false, fmt.Errorf("invalid session file: %w", err)
	}
	return id, true, nil
}

func saveSessionIDFile(path, rawID string) (string, error) {
	id, err := normalizeSessionID(rawID)
	if err != nil {
		return "", err
	}
	if err := atomicfile.WriteFile(path, []byte(id), 0o600); err != nil {
		return "", fmt.Errorf("write session file: %w", err)
	}
	return id, nil
}

func touchSessionFile(path string) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return
	}
	now := time.Now()
	_ = os.Chtimes(path, now, now)
}

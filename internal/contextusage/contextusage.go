// Package contextusage records actual context tokens consumed by agent flows.
package contextusage

import (
	"bufio"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Entry is one context event in a patch session.
type Entry struct {
	Timestamp      time.Time `json:"timestamp"`
	SessionID      string    `json:"session_id"`
	Phase          string    `json:"phase"`
	Command        string    `json:"command"`
	Query          string    `json:"query,omitempty"`
	Path           string    `json:"path,omitempty"`
	Mode           string    `json:"mode,omitempty"`
	StartLine      int       `json:"start_line,omitempty"`
	EndLine        int       `json:"end_line,omitempty"`
	Hash           string    `json:"hash,omitempty"`
	Tokens         int       `json:"tokens"`
	BaselineTokens int       `json:"baseline_tokens,omitempty"`
	Bytes          int       `json:"bytes,omitempty"`
}

// Summary aggregates entries for a session.
type Summary struct {
	SessionID          string      `json:"session_id"`
	InitialTokens      int         `json:"initial_tokens"`
	ExpansionTokens    int         `json:"expansion_tokens"`
	TotalTokens        int         `json:"total_tokens"`
	Expansions         int         `json:"expansions"`
	ExpandedFiles      int         `json:"expanded_files"`
	FullFileExpansions int         `json:"full_file_expansions"`
	BaselineTokens     int         `json:"baseline_tokens,omitempty"`
	EstimatedSaved     int         `json:"estimated_saved_tokens,omitempty"`
	SavingsRatio       float64     `json:"savings_ratio,omitempty"`
	Files              []FileUsage `json:"files,omitempty"`
	OverExpandedFiles  []string    `json:"over_expanded_files,omitempty"`
	Recommendations    []string    `json:"recommendations,omitempty"`
}

// FileUsage aggregates expansion events for one path.
type FileUsage struct {
	Path               string   `json:"path"`
	ExpansionTokens    int      `json:"expansion_tokens"`
	Expansions         int      `json:"expansions"`
	FullFileExpansions int      `json:"full_file_expansions,omitempty"`
	Modes              []string `json:"modes,omitempty"`
	Ranges             []string `json:"ranges,omitempty"`
	Hashes             []string `json:"hashes,omitempty"`
	Bytes              int      `json:"bytes,omitempty"`
}

// Path returns the repo-local context usage log path.
func Path(repoRoot string) string {
	return filepath.Join(repoRoot, ".neurofs", "context_usage.jsonl")
}

// NewSessionID creates a stable-looking compact id for one patch-context run.
func NewSessionID(query string, now time.Time) string {
	sum := sha1.Sum([]byte(query + "|" + now.UTC().Format(time.RFC3339Nano)))
	return hex.EncodeToString(sum[:])[:12]
}

// Append writes one usage entry.
func Append(repoRoot string, entry Entry) error {
	if entry.Timestamp.IsZero() {
		entry.Timestamp = time.Now().UTC()
	}
	p := Path(repoRoot)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(enc, '\n')); err != nil {
		return err
	}
	return nil
}

// Read loads usage entries, optionally filtering by session id.
func Read(repoRoot, sessionID string) ([]Entry, error) {
	p := Path(repoRoot)
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var entries []Entry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var entry Entry
		if err := json.Unmarshal(sc.Bytes(), &entry); err != nil {
			return nil, fmt.Errorf("decode %s: %w", p, err)
		}
		if sessionID == "" || entry.SessionID == sessionID {
			entries = append(entries, entry)
		}
	}
	return entries, sc.Err()
}

// Summarise aggregates one session. BaselineTokens can be the full-file
// token cost for selected files to estimate savings against eager reading.
func Summarise(sessionID string, entries []Entry, baselineTokens int) Summary {
	s := Summary{SessionID: sessionID}
	byPath := make(map[string]*FileUsage)
	providedBaseline := baselineTokens
	for _, entry := range entries {
		if s.SessionID == "" {
			s.SessionID = entry.SessionID
		}
		switch entry.Phase {
		case "initial_bundle":
			s.InitialTokens += entry.Tokens
		case "expansion":
			s.ExpansionTokens += entry.Tokens
			s.Expansions++
			if entry.Path != "" {
				file := byPath[entry.Path]
				if file == nil {
					file = &FileUsage{Path: entry.Path}
					byPath[entry.Path] = file
				}
				file.ExpansionTokens += entry.Tokens
				file.Expansions++
				file.Bytes += entry.Bytes
				file.Modes = appendUnique(file.Modes, entry.Mode)
				if entry.StartLine > 0 && entry.EndLine >= entry.StartLine {
					file.Ranges = appendUnique(file.Ranges, fmt.Sprintf("%d-%d", entry.StartLine, entry.EndLine))
				}
				file.Hashes = appendUnique(file.Hashes, entry.Hash)
				if entry.Mode == "full" {
					file.FullFileExpansions++
					s.FullFileExpansions++
				}
			}
		default:
			s.ExpansionTokens += entry.Tokens
		}
		if providedBaseline == 0 && entry.BaselineTokens > 0 {
			baselineTokens += entry.BaselineTokens
		}
	}
	s.TotalTokens = s.InitialTokens + s.ExpansionTokens
	s.Files = sortedFileUsage(byPath)
	s.ExpandedFiles = len(s.Files)
	for _, file := range s.Files {
		if file.FullFileExpansions > 0 {
			s.OverExpandedFiles = append(s.OverExpandedFiles, file.Path)
			s.Recommendations = append(s.Recommendations,
				fmt.Sprintf("%s used full-file expansion; prefer outline or hashed excerpts when possible", file.Path))
		}
		if file.Expansions > 3 {
			s.Recommendations = append(s.Recommendations,
				fmt.Sprintf("%s expanded %d times; consider one broader excerpt or full outline", file.Path, file.Expansions))
		}
	}
	if baselineTokens > 0 {
		s.BaselineTokens = baselineTokens
	}
	if baselineTokens > 0 {
		s.EstimatedSaved = baselineTokens - s.TotalTokens
		if baselineTokens > 0 {
			s.SavingsRatio = float64(s.TotalTokens) / float64(baselineTokens)
		}
	}
	return s
}

func appendUnique(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func sortedFileUsage(byPath map[string]*FileUsage) []FileUsage {
	if len(byPath) == 0 {
		return nil
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	out := make([]FileUsage, 0, len(paths))
	for _, path := range paths {
		out = append(out, *byPath[path])
	}
	return out
}

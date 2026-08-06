package orchestrator

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CrossProjectSkill represents a domain insight learned from one repo that benefits all repos.
type CrossProjectSkill struct {
	ID               string    `json:"id"`
	Domain           string    `json:"domain"`    // "go_cli", "nextjs", "python_api"
	TaskKind         TaskKind  `json:"task_kind"` // backend, database, etc.
	RecommendedModel string    `json:"recommended_model"`
	Insight          string    `json:"insight"`
	Confidence       float64   `json:"confidence"` // 0.0 - 1.0
	LearnedAt        time.Time `json:"learned_at"`
}

// SkillStore manages the ~/.neurofs/global_skills.jsonl cross-project memory.
type SkillStore struct {
	path string
}

// skillStoreMu serialises access per store path. It is package-level on
// purpose: callers construct a fresh SkillStore per HTTP request, so a mutex
// on the struct would be a new lock every time and guard nothing.
var (
	skillStoreMu   sync.Mutex
	skillStoreLock = map[string]*sync.Mutex{}
)

func lockForPath(path string) *sync.Mutex {
	skillStoreMu.Lock()
	defer skillStoreMu.Unlock()
	if m, ok := skillStoreLock[path]; ok {
		return m
	}
	m := &sync.Mutex{}
	skillStoreLock[path] = m
	return m
}

// NewSkillStore creates or connects to the global skills store in the given directory.
func NewSkillStore(dir string) *SkillStore {
	if dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".neurofs")
		} else {
			dir = "."
		}
	}
	_ = os.MkdirAll(dir, 0o755)
	return &SkillStore{
		path: filepath.Join(dir, "global_skills.jsonl"),
	}
}

// SaveSkill appends a cross-project skill to global_skills.jsonl.
func (ss *SkillStore) SaveSkill(skill CrossProjectSkill) error {
	if ss == nil || ss.path == "" {
		return nil
	}
	mu := lockForPath(ss.path)
	mu.Lock()
	defer mu.Unlock()

	if skill.LearnedAt.IsZero() {
		skill.LearnedAt = time.Now()
	}

	data, err := json.Marshal(skill)
	if err != nil {
		return err
	}

	f, err := os.OpenFile(ss.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	_, err = f.Write(append(data, '\n'))
	return err
}

// LoadSkills returns all stored cross-project skills.
func (ss *SkillStore) LoadSkills() ([]CrossProjectSkill, error) {
	if ss == nil || ss.path == "" {
		return nil, nil
	}
	mu := lockForPath(ss.path)
	mu.Lock()
	defer mu.Unlock()

	f, err := os.Open(ss.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var skills []CrossProjectSkill
	scanner := bufio.NewScanner(f)
	// Insights are free text, so a row can exceed bufio's 64KB default. Without
	// a bigger buffer the scan stops at the first long line and, with the error
	// dropped, silently returns a truncated store as if it were complete.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) == 0 {
			continue
		}
		var skill CrossProjectSkill
		if err := json.Unmarshal([]byte(line), &skill); err == nil {
			skills = append(skills, skill)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read skill store %s: %w", ss.path, err)
	}
	return skills, nil
}

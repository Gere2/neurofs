package orchestrator

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// CrossProjectSkill represents a domain insight learned from one repo that benefits all repos.
type CrossProjectSkill struct {
	ID               string    `json:"id"`
	Domain           string    `json:"domain"`           // "go_cli", "nextjs", "python_api"
	TaskKind         TaskKind  `json:"task_kind"`        // backend, database, etc.
	RecommendedModel string    `json:"recommended_model"`
	Insight          string    `json:"insight"`
	Confidence       float64   `json:"confidence"`       // 0.0 - 1.0
	LearnedAt        time.Time `json:"learned_at"`
}

// SkillStore manages the ~/.neurofs/global_skills.jsonl cross-project memory.
type SkillStore struct {
	mu   sync.Mutex
	path string
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
	ss.mu.Lock()
	defer ss.mu.Unlock()

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
	defer f.Close()

	_, err = f.Write(append(data, '\n'))
	return err
}

// LoadSkills returns all stored cross-project skills.
func (ss *SkillStore) LoadSkills() ([]CrossProjectSkill, error) {
	if ss == nil || ss.path == "" {
		return nil, nil
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()

	f, err := os.Open(ss.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var skills []CrossProjectSkill
	scanner := bufio.NewScanner(f)
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
	return skills, nil
}

package orchestrator

import (
	"fmt"
	"strings"
)

// Router resolves sub-tasks to available LLM models based on routing rules and availability.
type Router struct {
	Config ModelsConfig
}

// NewRouter creates a router backed by the given ModelsConfig.
func NewRouter(cfg ModelsConfig) *Router {
	return &Router{Config: cfg}
}

// ResolvedModel contains the model entry and name chosen for a task.
type ResolvedModel struct {
	Name  string
	Entry ModelEntry
}

// SelectModel picks the best model for a given sub-task based on explicit override,
// complexity escalation, kind mapping, or fallback default.
func (r *Router) SelectModel(task Task) (ResolvedModel, error) {
	if len(r.Config.Models) == 0 {
		r.Config = DefaultModelsConfig()
	}

	// 1. Explicit task model override
	if task.Model != "" {
		if entry, ok := r.Config.Models[task.Model]; ok {
			return ResolvedModel{Name: task.Model, Entry: entry}, nil
		}
	}

	// 2. Escalation for Complex tasks
	if task.Complexity == Complex {
		if complexModel, ok := r.Config.Routing["complex"]; ok {
			if entry, ok := r.Config.Models[complexModel]; ok {
				return ResolvedModel{Name: complexModel, Entry: entry}, nil
			}
		}
	}

	// 3. Routing rule by TaskKind
	kindKey := strings.ToLower(string(task.Kind))
	if modelName, ok := r.Config.Routing[kindKey]; ok {
		if entry, ok := r.Config.Models[modelName]; ok {
			return ResolvedModel{Name: modelName, Entry: entry}, nil
		}
	}

	// 4. Default routing rule fallback
	if defaultModel, ok := r.Config.Routing["default"]; ok {
		if entry, ok := r.Config.Models[defaultModel]; ok {
			return ResolvedModel{Name: defaultModel, Entry: entry}, nil
		}
	}

	// 5. Ultimate fallback to any available model in config
	for name, entry := range r.Config.Models {
		return ResolvedModel{Name: name, Entry: entry}, nil
	}

	return ResolvedModel{}, fmt.Errorf("no models configured for routing task kind %q", task.Kind)
}

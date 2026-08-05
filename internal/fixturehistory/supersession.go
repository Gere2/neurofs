// Package fixturehistory validates append-only correction links between
// governance fixtures.
package fixturehistory

import (
	"fmt"
	"path/filepath"
	"strings"
)

// Entry is the history metadata needed to decide whether a fixture is active.
// Name must be the fixture's basename. Supersedes, when present, names one
// older fixture in the same directory.
type Entry struct {
	Name       string
	Supersedes string
}

// Active returns the basenames that should participate in evaluation.
//
// Corrections are append-only: a correction remains in the directory and
// names the obsolete fixture through Supersedes. Invalid, missing, ambiguous,
// or cyclic links fail closed so a typo cannot silently remove gate evidence.
func Active(entries []Entry) (map[string]bool, error) {
	byName := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		if !validBasename(name) {
			return nil, fmt.Errorf("invalid fixture name %q", entry.Name)
		}
		if _, exists := byName[name]; exists {
			return nil, fmt.Errorf("duplicate fixture name %q", name)
		}
		entry.Name = name
		entry.Supersedes = strings.TrimSpace(entry.Supersedes)
		byName[name] = entry
	}

	// edges point from correction to the older entry it replaces.
	edges := make(map[string]string)
	replacedBy := make(map[string]string)
	for name, entry := range byName {
		target := entry.Supersedes
		if target == "" {
			continue
		}
		if !validBasename(target) {
			return nil, fmt.Errorf("%s: invalid supersedes target %q", name, target)
		}
		if target == name {
			return nil, fmt.Errorf("%s: fixture cannot supersede itself", name)
		}
		if _, exists := byName[target]; !exists {
			return nil, fmt.Errorf("%s: supersedes target %q does not exist", name, target)
		}
		if other, exists := replacedBy[target]; exists {
			return nil, fmt.Errorf(
				"%s and %s both supersede %s",
				other,
				name,
				target,
			)
		}
		edges[name] = target
		replacedBy[target] = name
	}

	const (
		unseen = iota
		visiting
		done
	)
	state := make(map[string]int, len(byName))
	var visit func(string) error
	visit = func(name string) error {
		switch state[name] {
		case visiting:
			return fmt.Errorf("supersession cycle involving %s", name)
		case done:
			return nil
		}
		state[name] = visiting
		if target := edges[name]; target != "" {
			if err := visit(target); err != nil {
				return err
			}
		}
		state[name] = done
		return nil
	}
	for name := range byName {
		if err := visit(name); err != nil {
			return nil, err
		}
	}

	active := make(map[string]bool, len(byName)-len(replacedBy))
	for name := range byName {
		if _, superseded := replacedBy[name]; !superseded {
			active[name] = true
		}
	}
	return active, nil
}

func validBasename(name string) bool {
	return name != "" &&
		name == filepath.Base(name) &&
		!strings.ContainsAny(name, `/\`) &&
		strings.HasSuffix(strings.ToLower(name), ".json")
}

package ranking

import (
	"path/filepath"
	"strings"
	"unicode"
)

// Compat/legacy layers mirror the current API on purpose: vuejs/core's
// vue-compat package re-declares `Vue`, `setup`, and friends as thin
// stubs, and those declarations outscore the real implementations on
// symbol identity for ordinary questions (measured: the component-setup
// fixture's misses are all compat-layer hits). Like test files, legacy
// paths are what you want only when you ask for them.
var legacyLikeDirTokens = map[string]bool{
	"compat":     true,
	"legacy":     true,
	"deprecated": true,
	"polyfill":   true,
	"polyfills":  true,
	"shims":      true,
}

// legacyOverrideTerms make QueryWantsLegacy return true so the downrank is
// lifted when the question is actually about the legacy surface.
var legacyOverrideTerms = map[string]bool{
	"compat":        true,
	"compatibility": true,
	"legacy":        true,
	"deprecated":    true,
	"deprecation":   true,
	"migration":     true,
	"backwards":     true,
}

// IsLegacyLikePath reports whether relPath sits under a compat/legacy
// layer. Directory names are matched token-wise (split on non-alphanumeric
// characters) so `vue-compat`, `compat`, and `legacy-utils` all count while
// `compatibility` does not; file basenames are not inspected — a file
// named compat.go in a live package is usually the live implementation.
func IsLegacyLikePath(relPath string) bool {
	rel := strings.ToLower(filepath.ToSlash(relPath))
	if rel == "" {
		return false
	}
	segments := strings.Split(rel, "/")
	if len(segments) < 2 {
		return false
	}
	for _, dir := range segments[:len(segments)-1] {
		for _, token := range splitPathTokens(dir) {
			if legacyLikeDirTokens[token] {
				return true
			}
		}
	}
	return false
}

// QueryWantsLegacy reports whether the query names the legacy/compat
// surface explicitly, which lifts the downrank.
func QueryWantsLegacy(query string) bool {
	if query == "" {
		return false
	}
	words := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	})
	for _, w := range words {
		if legacyOverrideTerms[w] {
			return true
		}
	}
	return false
}

func splitPathTokens(segment string) []string {
	return strings.FieldsFunc(segment, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

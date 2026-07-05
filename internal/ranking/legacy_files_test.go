package ranking

import "testing"

func TestIsLegacyLikePath(t *testing.T) {
	cases := map[string]bool{
		"packages/vue-compat/src/index.ts":           true, // dashed dir token
		"packages/runtime-core/src/compat/global.ts": true,
		"src/legacy/router.js":                       true,
		"lib/polyfills/promise.js":                   true,
		"internal/retrieval/search.go":               false,
		"src/compatibility/matrix.ts":                false, // token is "compatibility", not "compat"
		"compat.go":                                  false, // basename only, no legacy dir
		"":                                           false,
	}
	for path, want := range cases {
		if got := IsLegacyLikePath(path); got != want {
			t.Errorf("IsLegacyLikePath(%q) = %v, want %v", path, got, want)
		}
	}
}

func TestQueryWantsLegacy(t *testing.T) {
	cases := map[string]bool{
		"How does the compat layer emulate Vue 2?": true,
		"what was deprecated in the migration":     true,
		"How does the renderer mount components?":  false,
		"": false,
	}
	for query, want := range cases {
		if got := QueryWantsLegacy(query); got != want {
			t.Errorf("QueryWantsLegacy(%q) = %v, want %v", query, got, want)
		}
	}
}

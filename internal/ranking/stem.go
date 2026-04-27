package ranking

import "strings"

// stemWord collapses a small set of English plural and derivational suffixes
// so the ranker can treat "utility" and "utils" — or "helpers" and "helper" —
// as the same key. It is intentionally tiny and deterministic: we do not ship
// a full Porter stemmer because every reduction we apply must be obvious at
// a glance so unusual scoring decisions remain auditable.
//
// Rules, applied as at most one inflectional step followed by one
// derivational step:
//
//	ies → y        queries → query
//	sses → ss      classes → class
//	ss  → (leave)  class, pass, boss
//	s   → (drop)   utils → util, helpers → helper
//	ity → (drop)   utility → util
//	ing → (drop)   hashing → hash       (requires length > 5)
//	ed  → (drop)   registered → register (requires length > 4)
//
// Words shorter than 4 characters pass through unchanged so short identifiers
// (api, go, ts) are not mangled. The result is a stable matching key, not a
// linguistically correct root.
func stemWord(w string) string {
	if len(w) < 4 {
		return w
	}
	w = strings.ToLower(w)

	switch {
	case strings.HasSuffix(w, "ies") && len(w) > 4:
		w = w[:len(w)-3] + "y"
	case strings.HasSuffix(w, "sses"):
		w = w[:len(w)-2]
	case strings.HasSuffix(w, "ss"):
		// leave alone: class, pass, boss
	case strings.HasSuffix(w, "s") && len(w) > 3:
		w = w[:len(w)-1]
	}

	switch {
	case strings.HasSuffix(w, "ity") && len(w) > 4:
		w = w[:len(w)-3]
	case strings.HasSuffix(w, "ing") && len(w) > 5:
		w = w[:len(w)-3]
	case strings.HasSuffix(w, "ed") && len(w) > 4:
		w = w[:len(w)-2]
	}

	return w
}

// termVariants returns the set of string forms used when matching a query
// term against a candidate identifier. It is always at least one element and
// at most two: the original term and its stem. Duplicates and empty strings
// are filtered out so callers can iterate without extra checks.
func termVariants(term string) []string {
	term = strings.ToLower(term)
	stem := stemWord(term)
	if stem == "" || stem == term {
		return []string{term}
	}
	return []string{term, stem}
}

// anyContains reports whether haystack contains any of the needles. It is a
// small helper to keep scoreFile readable when we match a term together with
// its stem against a single target.
func anyContains(haystack string, needles []string) bool {
	for _, n := range needles {
		if n != "" && strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// anyContainsReverse is the dual of anyContains: it reports whether any of
// the provided strings (typically term variants) contains the given needle.
// This handles cases such as the file stem "auth" appearing inside the query
// term "authentication".
func anyContainsReverse(haystacks []string, needle string) bool {
	if needle == "" {
		return false
	}
	for _, h := range haystacks {
		if strings.Contains(h, needle) {
			return true
		}
	}
	return false
}

// Stem and TermVariants are exported wrappers so other packages (notably
// internal/packager when extracting sub-file excerpts) can reuse the exact
// normalisation the ranker applies to query terms. Keeping the rule set
// behind a single implementation prevents the two layers from drifting:
// if the ranker considers "rendering" → "render" the same identifier,
// excerpt extraction must too, otherwise a top-ranked file would produce
// a signature instead of an excerpt for a query the ranker matched.
//
// We expose wrappers (rather than renaming the package-private originals)
// to keep all existing intra-package call sites untouched. The wrappers
// have no logic of their own — they exist solely as a stable public API.
func Stem(w string) string { return stemWord(w) }

// TermVariants is the public form of the package-internal termVariants. See
// Stem for the rationale.
func TermVariants(term string) []string { return termVariants(term) }

package audit

import "strings"

// ScoreFacts checks whether each expected fact appears (case-insensitively)
// as a substring of the response. Returns the hits and the recall ratio.
// Empty fact sets (nil, or a slice where every entry is blank after trim)
// return (nil, 1.0) — "no expectations, full credit" — so a question without
// real facts does not penalise the aggregate.
//
// Substring matching is intentional: the benchmark author writes facts as
// short anchors ("jwt.sign", "decrement stock") and the model is free to
// phrase around them. A stricter (BM25, embedding) scorer is a later knob,
// not a v1 requirement.
func ScoreFacts(response string, facts []string) ([]string, float64) {
	// Filter blanks up front so they neither count as hits nor inflate the
	// denominator. A caller passing ["jwt.sign", ""] gets the same recall
	// as one passing ["jwt.sign"] — the blank entry was never a real
	// expectation in the first place.
	valid := make([]string, 0, len(facts))
	for _, f := range facts {
		if strings.TrimSpace(f) != "" {
			valid = append(valid, f)
		}
	}
	if len(valid) == 0 {
		return nil, 1.0
	}
	lower := strings.ToLower(response)

	hits := make([]string, 0, len(valid))
	for _, f := range valid {
		needle := strings.ToLower(strings.TrimSpace(f))
		if strings.Contains(lower, needle) {
			hits = append(hits, f)
		}
	}
	return hits, float64(len(hits)) / float64(len(valid))
}

package audit

import (
	"regexp"
	"strings"
)

// ScoreFacts checks whether each expected fact appears (case-insensitively)
// in the response, using word-boundary matching for facts whose ends are
// word characters. Returns the hits and the recall ratio. Empty fact sets
// (nil, or a slice where every entry is blank after trim) return (nil, 1.0)
// — "no expectations, full credit" — so a question without real facts does
// not penalise the aggregate.
//
// Word-boundary matching prevents the false-PASS the DevX agent surfaced:
// the fact "weightFilename" must NOT match the renamed identifier
// "weightFilenameRenamed". For facts whose first/last character is not a
// word char (e.g. "=>"), the boundary is dropped on that side so
// punctuation-anchor facts still match.
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

	hits := make([]string, 0, len(valid))
	for _, f := range valid {
		re, err := compileFactRegex(strings.TrimSpace(f))
		if err != nil || re == nil {
			continue
		}
		if re.MatchString(response) {
			hits = append(hits, f)
		}
	}
	return hits, float64(len(hits)) / float64(len(valid))
}

func compileFactRegex(needle string) (*regexp.Regexp, error) {
	if needle == "" {
		return nil, nil
	}
	pattern := regexp.QuoteMeta(needle)
	if isWordByte(needle[0]) {
		pattern = `\b` + pattern
	}
	if isWordByte(needle[len(needle)-1]) {
		pattern = pattern + `\b`
	}
	return regexp.Compile(`(?i)` + pattern)
}

func isWordByte(b byte) bool {
	return (b >= 'a' && b <= 'z') ||
		(b >= 'A' && b <= 'Z') ||
		(b >= '0' && b <= '9') ||
		b == '_'
}

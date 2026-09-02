package mcp

import (
	"path"
	"strings"
	"unicode"
)

// DomainWarningText is the hint attached to a response whose query is asking
// for visual or binary assets. NeuroFS indexes source code: those files are
// never in the index, so the ranked code it returns instead is noise the
// caller should not spend a turn reading.
const DomainWarningText = "This query appears to target visual/binary assets. " +
	"NeuroFS indexes source code. Consider using find or rg on output/, assets/ " +
	"or public/ directories."

// visualDomainTerms are the words that mark a query as asking for design
// output rather than code. Measured origin: every "no" rating in 152 real
// raiz-app feedbacks came from this domain — poster/brand-asset questions
// answered with TypeScript. Spanish and English are both listed because the
// agents querying this corpus mix them.
var visualDomainTerms = []string{
	"cartel", "poster", "logo", "marca", "visual", "png", "svg",
	"font", "tipografía", "tipografia", "color palette", "icono",
	"ilustración", "ilustracion", "asset", "imagen",
}

// assetDirectories hold the outputs those queries are actually after. A top
// hit inside one of them means retrieval already pointed at the right place,
// so the warning would be wrong.
var assetDirectories = map[string]bool{
	"output": true, "assets": true, "public": true, "creative": true,
}

// sourceCodeExtensions is what "NeuroFS answered with code" looks like.
// Documentation is deliberately absent: a .md file about brand guidelines is
// a legitimate answer to a branding question.
var sourceCodeExtensions = map[string]bool{
	".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true,
	".cjs": true, ".go": true, ".py": true, ".rb": true, ".java": true,
	".rs": true, ".c": true, ".h": true, ".cc": true, ".cpp": true,
	".hpp": true, ".cs": true, ".php": true, ".swift": true, ".kt": true,
	".scala": true, ".vue": true, ".svelte": true, ".sh": true, ".sql": true,
}

// domainWarningFor returns the non-code domain hint when a query names at
// least two visual-asset terms and the best result NeuroFS could serve is
// source code from outside any asset directory. The search still runs and
// still returns its results — this is an extra hint, never a block.
func domainWarningFor(query, topPath string) string {
	if countVisualDomainTerms(query) < 2 {
		return ""
	}
	if !isSourceCodePath(topPath) || pathInAssetDirectory(topPath) {
		return ""
	}
	return DomainWarningText
}

// countVisualDomainTerms counts distinct visual-domain terms in the query.
// Single words match a whole query token or a token that extends it, so
// Spanish plural and gender variants ("visuales", "iconos") count; terms
// containing a space are matched against the raw query.
func countVisualDomainTerms(query string) int {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return 0
	}
	tokens := strings.FieldsFunc(query, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	count := 0
	for _, term := range visualDomainTerms {
		if strings.Contains(term, " ") {
			if strings.Contains(query, term) {
				count++
			}
			continue
		}
		for _, token := range tokens {
			if strings.HasPrefix(token, term) {
				count++
				break
			}
		}
	}
	return count
}

func isSourceCodePath(relPath string) bool {
	return sourceCodeExtensions[strings.ToLower(path.Ext(strings.TrimSpace(relPath)))]
}

func pathInAssetDirectory(relPath string) bool {
	for _, segment := range strings.Split(path.Clean(strings.TrimSpace(relPath)), "/") {
		if assetDirectories[strings.ToLower(segment)] {
			return true
		}
	}
	return false
}

// topResultPath returns the path of the highest-ranked hit, or "" when the
// search returned nothing to judge.
func topResultPath(hits []SearchResultHit) string {
	if len(hits) == 0 {
		return ""
	}
	return hits[0].Path
}

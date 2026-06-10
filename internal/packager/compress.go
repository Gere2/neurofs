package packager

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
)

var licenseKeywords = []string{
	"copyright",
	"license",
	"spdx-license-identifier",
	"apache",
	"created by",
	"author",
	"all rights reserved",
}

// CompressCode strips leading license/copyright comments (or all comments if stripComments is true),
// collapses blank lines (or removes them entirely if stripBlankLines is true),
// and compresses leading indentation for brace-based languages.
func CompressCode(lang models.Lang, content string, stripComments, stripBlankLines bool) string {
	if lang == models.LangJSON {
		if minified, err := compressJSON(content); err == nil {
			return minified
		}
	}

	if stripComments {
		content = stripAllComments(lang, content)
	} else {
		content = stripLicenseHeader(content)
	}

	if stripBlankLines {
		content = collapseAllBlankLines(content)
	} else {
		content = collapseBlankLines(content)
	}

	content = compressIndentation(lang, content)
	return content
}

// stripLicenseHeader removes leading comments containing copyright/license terms.
func stripLicenseHeader(content string) string {
	trimmed := strings.TrimSpace(content)
	if trimmed == "" {
		return content
	}

	// Helper to check if a text contains any license keywords
	containsLicenseKeyword := func(text string) bool {
		lower := strings.ToLower(text)
		for _, kw := range licenseKeywords {
			if strings.Contains(lower, kw) {
				return true
			}
		}
		return false
	}

	// Iterate and strip leading comment blocks as long as they contain license keywords.
	for {
		changed := false

		// 1. Check C-style block comment: /* ... */
		if strings.HasPrefix(trimmed, "/*") {
			endIdx := strings.Index(trimmed, "*/")
			if endIdx != -1 {
				commentContent := trimmed[2:endIdx]
				if containsLicenseKeyword(commentContent) {
					trimmed = strings.TrimSpace(trimmed[endIdx+2:])
					changed = true
				}
			}
		}

		// 2. Check C-style line comments: // ...
		if !changed && strings.HasPrefix(trimmed, "//") {
			lines := strings.Split(trimmed, "\n")
			commentEndLine := 0
			commentText := ""
			for i, line := range lines {
				lineTrimmed := strings.TrimSpace(line)
				if strings.HasPrefix(lineTrimmed, "//") {
					commentEndLine = i + 1
					commentText += "\n" + lineTrimmed
				} else if lineTrimmed == "" {
					// Allow single blank lines within consecutive line comments
					commentEndLine = i + 1
				} else {
					break
				}
			}
			if commentEndLine > 0 && containsLicenseKeyword(commentText) {
				trimmed = strings.TrimSpace(strings.Join(lines[commentEndLine:], "\n"))
				changed = true
			}
		}

		// 3. Check Script-style line comments: # ...
		if !changed && strings.HasPrefix(trimmed, "#") {
			lines := strings.Split(trimmed, "\n")
			commentEndLine := 0
			commentText := ""
			for i, line := range lines {
				lineTrimmed := strings.TrimSpace(line)
				if strings.HasPrefix(lineTrimmed, "#") {
					commentEndLine = i + 1
					commentText += "\n" + lineTrimmed
				} else if lineTrimmed == "" {
					commentEndLine = i + 1
				} else {
					break
				}
			}
			if commentEndLine > 0 && containsLicenseKeyword(commentText) {
				trimmed = strings.TrimSpace(strings.Join(lines[commentEndLine:], "\n"))
				changed = true
			}
		}

		if !changed {
			break
		}
	}

	return trimmed
}

var consecutiveNewlinesRegex = regexp.MustCompile(`\n{3,}`)

// collapseBlankLines normalizes newlines, trims trailing space per line, and collapses multiple blank lines to one.
func collapseBlankLines(content string) string {
	// Normalize line endings to \n
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	lines := strings.Split(content, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	content = strings.Join(lines, "\n")

	// Collapse 3 or more consecutive newlines into 2 (one blank line)
	content = consecutiveNewlinesRegex.ReplaceAllString(content, "\n\n")

	return strings.TrimSpace(content)
}

// collapseAllBlankLines removes all empty lines from the content entirely.
func collapseAllBlankLines(content string) string {
	// Normalize line endings to \n
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")

	lines := strings.Split(content, "\n")
	var nonBlank []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			nonBlank = append(nonBlank, strings.TrimRight(line, " \t"))
		}
	}
	return strings.Join(nonBlank, "\n")
}

// compressIndentation converts tabs to 2 spaces and halves even leading-space indentations for braces-based languages.
func compressIndentation(lang models.Lang, content string) string {
	// Skip indentation-sensitive or format-preserving languages
	if lang == models.LangPython || lang == models.LangMarkdown || lang == models.LangJSON || lang == models.LangYAML {
		return content
	}

	lines := strings.Split(content, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		// Count leading spaces and tabs
		runes := []rune(line)
		spaces := 0
		tabs := 0
		idx := 0
		for idx < len(runes) {
			if runes[idx] == ' ' {
				spaces++
				idx++
			} else if runes[idx] == '\t' {
				tabs++
				idx++
			} else {
				break
			}
		}

		if tabs > 0 && spaces == 0 {
			lines[i] = strings.Repeat("  ", tabs) + string(runes[idx:])
		} else if spaces >= 2 && spaces%2 == 0 && tabs == 0 {
			lines[i] = strings.Repeat(" ", spaces/2) + string(runes[idx:])
		}
	}
	return strings.Join(lines, "\n")
}

// compressJSON minifies a JSON string using the standard encoding/json library.
func compressJSON(content string) (string, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(content)); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// stripAllComments removes block and line comments from the content for supported languages
// while avoiding comments inside string literals, and preserving Go compilation directives.
func stripAllComments(lang models.Lang, content string) string {
	var sb strings.Builder
	runes := []rune(content)
	n := len(runes)
	i := 0

	inString := false
	var quoteChar rune
	inBlockComment := false
	inLineComment := false

	// Python triple quote tracking
	inPythonTripleSingle := false
	inPythonTripleDouble := false

	for i < n {
		r := runes[i]

		if inLineComment {
			if r == '\n' {
				inLineComment = false
				sb.WriteRune(r)
			}
			i++
			continue
		}

		if inBlockComment {
			if r == '*' && i+1 < n && runes[i+1] == '/' {
				inBlockComment = false
				i += 2
			} else {
				i++
			}
			continue
		}

		if lang == models.LangPython {
			if inPythonTripleSingle {
				if r == '\'' && i+2 < n && runes[i+1] == '\'' && runes[i+2] == '\'' {
					inPythonTripleSingle = false
					sb.WriteString("'''")
					i += 3
				} else {
					sb.WriteRune(r)
					i++
				}
				continue
			}
			if inPythonTripleDouble {
				if r == '"' && i+2 < n && runes[i+1] == '"' && runes[i+2] == '"' {
					inPythonTripleDouble = false
					sb.WriteString(`"""`)
					i += 3
				} else {
					sb.WriteRune(r)
					i++
				}
				continue
			}
		}

		if inString {
			if r == '\\' && i+1 < n {
				sb.WriteRune(r)
				sb.WriteRune(runes[i+1])
				i += 2
				continue
			}
			if r == quoteChar {
				inString = false
			}
			sb.WriteRune(r)
			i++
			continue
		}

		// Check for comment starts
		if lang == models.LangGo || lang == models.LangTypeScript || lang == models.LangJavaScript {
			if r == '/' && i+1 < n {
				if runes[i+1] == '/' {
					// Check for Go directives: e.g., //go:embed or // +build
					isDirective := false
					if i+2 < n {
						rem := string(runes[i+2:])
						if strings.HasPrefix(rem, "go:") || strings.HasPrefix(rem, " +build") {
							isDirective = true
						}
					}

					if isDirective {
						sb.WriteRune('/')
						sb.WriteRune('/')
						i += 2
						continue
					}

					inLineComment = true
					i += 2
					continue
				}
				if runes[i+1] == '*' {
					inBlockComment = true
					i += 2
					continue
				}
			}
		}

		if lang == models.LangPython || lang == models.LangYAML {
			if r == '#' {
				inLineComment = true
				i++
				continue
			}
		}

		// Check for string starts
		if lang == models.LangPython {
			if r == '\'' && i+2 < n && runes[i+1] == '\'' && runes[i+2] == '\'' {
				inPythonTripleSingle = true
				sb.WriteString("'''")
				i += 3
				continue
			}
			if r == '"' && i+2 < n && runes[i+1] == '"' && runes[i+2] == '"' {
				inPythonTripleDouble = true
				sb.WriteString(`"""`)
				i += 3
				continue
			}
		}

		if r == '"' || r == '\'' || (r == '`' && lang != models.LangPython) {
			inString = true
			quoteChar = r
		}

		sb.WriteRune(r)
		i++
	}

	return sb.String()
}

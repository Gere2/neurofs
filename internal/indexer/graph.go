package indexer

import (
	"path/filepath"
	"sort"
	"strings"

	"github.com/Gere2/neurofs/internal/models"
)

var (
	relativeImportExtensions = []string{".ts", ".js", ".tsx", ".jsx", ".go", ".py"}
	relativeIndexFiles       = []string{"/index.ts", "/index.js", "/index.tsx", "/index.jsx"}
)

type relationDirectory struct {
	files []models.FileRecord
}

// relationLookup contains the indexes needed by BuildRelations. Both suffix
// indexes use path-component boundaries, matching the old "/"+import suffix
// checks without scanning every indexed file for every import.
type relationLookup struct {
	filesByRel map[string]models.FileRecord

	directories       []relationDirectory
	directoryExact    map[string][]int
	directoryBySuffix map[string][]int

	stemFiles    []models.FileRecord
	stemExact    map[string][]int
	stemBySuffix map[string][]int
}

// BuildRelations walks all indexed file records, resolves their imports to target file records,
// and returns the set of file relationships.
func BuildRelations(files []models.FileRecord) []models.FileRelation {
	lookup := newRelationLookup(files)

	var relations []models.FileRelation
	seen := make(map[string]struct{})

	addRelation := func(src, dest, relType string) {
		if src == dest {
			return
		}
		key := src + "|" + dest + "|" + relType
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			relations = append(relations, models.FileRelation{
				SourcePath: src,
				TargetPath: dest,
				RelType:    relType,
			})
		}
	}

	for _, f := range files {
		srcDir := filepath.ToSlash(filepath.Dir(f.RelPath))

		for _, imp := range f.Imports {
			imp = strings.TrimSpace(imp)
			if imp == "" {
				continue
			}

			// Case 1: Relative import (JS/TS, Python: starting with . or ..)
			if strings.HasPrefix(imp, ".") || strings.HasPrefix(imp, "..") {
				// Clean relative path from the source file's directory
				targetRel := filepath.ToSlash(filepath.Clean(filepath.Join(srcDir, imp)))

				// Try direct match or common extensions
				if target, ok := lookup.filesByRel[targetRel]; ok {
					addRelation(f.Path, target.Path, "import")
					continue
				}

				// Check extensions
				matched := false
				for _, ext := range relativeImportExtensions {
					if target, ok := lookup.filesByRel[targetRel+ext]; ok {
						addRelation(f.Path, target.Path, "import")
						matched = true
						break
					}
				}
				if matched {
					continue
				}

				// Check folder import (e.g. ./user resolves to ./user/index.ts)
				for _, ext := range relativeIndexFiles {
					if target, ok := lookup.filesByRel[targetRel+ext]; ok {
						addRelation(f.Path, target.Path, "import")
						matched = true
						break
					}
				}
				if matched {
					continue
				}
			} else {
				// Case 2: Absolute / non-relative import (Go packages or Node modules)
				// Clean package suffix
				cleanedImp := filepath.ToSlash(imp)

				// Look for suffix match in all folders or files in the index.
				// For example, Go import "github.com/Gere2/neurofs/internal/storage"
				// suffix-matches folder "internal/storage".
				// Python import "crypto" matches "crypto.py".

				// Suffix folder match. Match in either direction so we cover
				// short imports against deep folders (Python: "crypto" → "lib/crypto")
				// and long canonical imports against shallow folders
				// (Go: "github.com/x/y/internal/storage" → "internal/storage").
				matchedDirectories := matchingPathIDs(
					cleanedImp,
					lookup.directoryExact,
					lookup.directoryBySuffix,
				)
				for _, directoryID := range matchedDirectories {
					for _, target := range lookup.directories[directoryID].files {
						if target.Lang == f.Lang { // matching language packages
							addRelation(f.Path, target.Path, "import")
						}
					}
				}
				if len(matchedDirectories) > 0 {
					continue
				}

				// Suffix file match: same bidirectional logic.
				for _, stemID := range matchingPathIDs(
					cleanedImp,
					lookup.stemExact,
					lookup.stemBySuffix,
				) {
					addRelation(f.Path, lookup.stemFiles[stemID].Path, "import")
				}
			}
		}
	}

	return relations
}

func newRelationLookup(files []models.FileRecord) relationLookup {
	lookup := relationLookup{
		filesByRel:        make(map[string]models.FileRecord, len(files)),
		directoryExact:    make(map[string][]int),
		directoryBySuffix: make(map[string][]int),
		stemExact:         make(map[string][]int),
		stemBySuffix:      make(map[string][]int),
	}

	// Direct relative resolution historically uses the final record for a
	// duplicate relative path. Retain that behaviour.
	for _, file := range files {
		lookup.filesByRel[file.RelPath] = file
	}

	// Directory package targets retain input order. This makes ambiguous
	// suffix matches deterministic while preserving order within a package.
	directoryIDs := make(map[string]int)
	for _, file := range files {
		directoryPath := filepath.ToSlash(filepath.Dir(file.RelPath))
		directoryID, ok := directoryIDs[directoryPath]
		if !ok {
			directoryID = len(lookup.directories)
			directoryIDs[directoryPath] = directoryID
			lookup.directories = append(lookup.directories, relationDirectory{})
			addPathIndex(lookup.directoryExact, lookup.directoryBySuffix, directoryPath, directoryID)
		}
		lookup.directories[directoryID].files = append(lookup.directories[directoryID].files, file)
	}

	// The non-relative file fallback also historically used the final record
	// for duplicate relative paths. Enumerate unique paths in first-seen order
	// so output does not depend on Go map iteration.
	seenRelativePaths := make(map[string]struct{}, len(lookup.filesByRel))
	for _, file := range files {
		if _, ok := seenRelativePaths[file.RelPath]; ok {
			continue
		}
		seenRelativePaths[file.RelPath] = struct{}{}
		file = lookup.filesByRel[file.RelPath]
		stem := file.RelPath
		if ext := filepath.Ext(file.RelPath); ext != "" {
			stem = file.RelPath[:len(file.RelPath)-len(ext)]
		}
		stemID := len(lookup.stemFiles)
		lookup.stemFiles = append(lookup.stemFiles, file)
		addPathIndex(lookup.stemExact, lookup.stemBySuffix, stem, stemID)
	}

	return lookup
}

func addPathIndex(exact, bySuffix map[string][]int, path string, id int) {
	exact[path] = append(exact[path], id)
	for _, suffix := range componentSuffixes(path) {
		bySuffix[suffix] = append(bySuffix[suffix], id)
	}
}

// matchingPathIDs returns IDs whose indexed path equals importPath, ends in
// "/"+importPath, or is itself a component suffix of importPath. IDs are
// sorted by construction order so ambiguous imports have stable results.
func matchingPathIDs(importPath string, exact, bySuffix map[string][]int) []int {
	candidates := make(map[int]struct{})
	for _, id := range bySuffix[importPath] {
		candidates[id] = struct{}{}
	}
	suffixes := componentSuffixes(importPath)
	for _, suffix := range suffixes[1:] {
		for _, id := range exact[suffix] {
			candidates[id] = struct{}{}
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	ids := make([]int, 0, len(candidates))
	for id := range candidates {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// componentSuffixes returns path and every suffix beginning immediately after
// a slash. Keeping empty/doubled components reproduces strings.HasSuffix with
// a "/" boundary for non-normalised import strings.
func componentSuffixes(path string) []string {
	suffixes := []string{path}
	for offset := 0; offset < len(path); {
		slash := strings.IndexByte(path[offset:], '/')
		if slash < 0 {
			break
		}
		offset += slash + 1
		suffixes = append(suffixes, path[offset:])
	}
	return suffixes
}

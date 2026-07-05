package indexer

import (
	"path/filepath"
	"strings"

	"github.com/neuromfs/neuromfs/internal/models"
)

// BuildRelations walks all indexed file records, resolves their imports to target file records,
// and returns the set of file relationships.
func BuildRelations(files []models.FileRecord) []models.FileRelation {
	// Create lookup maps for fast matching
	fileMap := make(map[string]models.FileRecord)  // rel_path -> record
	dirMap := make(map[string][]models.FileRecord) // dir_path -> records
	for _, f := range files {
		fileMap[f.RelPath] = f
		dir := filepath.ToSlash(filepath.Dir(f.RelPath))
		dirMap[dir] = append(dirMap[dir], f)
	}

	var relations []models.FileRelation
	seen := make(map[string]bool)

	addRelation := func(src, dest, relType string) {
		if src == dest {
			return
		}
		key := src + "|" + dest + "|" + relType
		if !seen[key] {
			seen[key] = true
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
				if target, ok := fileMap[targetRel]; ok {
					addRelation(f.Path, target.Path, "import")
					continue
				}

				// Check extensions
				matched := false
				for _, ext := range []string{".ts", ".js", ".tsx", ".jsx", ".go", ".py"} {
					if target, ok := fileMap[targetRel+ext]; ok {
						addRelation(f.Path, target.Path, "import")
						matched = true
						break
					}
				}
				if matched {
					continue
				}

				// Check folder import (e.g. ./user resolves to ./user/index.ts)
				for _, ext := range []string{"/index.ts", "/index.js", "/index.tsx", "/index.jsx"} {
					if target, ok := fileMap[targetRel+ext]; ok {
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
				// For example, Go import "github.com/neuromfs/neuromfs/internal/storage"
				// suffix-matches folder "internal/storage".
				// Python import "crypto" matches "crypto.py".

				// Suffix folder match. Match in either direction so we cover
				// short imports against deep folders (Python: "crypto" → "lib/crypto")
				// and long canonical imports against shallow folders
				// (Go: "github.com/x/y/internal/storage" → "internal/storage").
				foundFolder := false
				for dir, dirFiles := range dirMap {
					if dir == cleanedImp ||
						strings.HasSuffix(dir, "/"+cleanedImp) ||
						strings.HasSuffix(cleanedImp, "/"+dir) {
						foundFolder = true
						for _, target := range dirFiles {
							if target.Lang == f.Lang { // matching language packages
								addRelation(f.Path, target.Path, "import")
							}
						}
					}
				}
				if foundFolder {
					continue
				}

				// Suffix file match: same bidirectional logic.
				for relPath, target := range fileMap {
					stem := relPath
					if ext := filepath.Ext(relPath); ext != "" {
						stem = relPath[:len(relPath)-len(ext)]
					}
					if stem == cleanedImp ||
						strings.HasSuffix(stem, "/"+cleanedImp) ||
						strings.HasSuffix(cleanedImp, "/"+stem) {
						addRelation(f.Path, target.Path, "import")
					}
				}
			}
		}
	}

	return relations
}

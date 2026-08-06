package ranking

import (
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/project"
)

func TestExpandByGraphRelationsMatchesNaiveScan(t *testing.T) {
	scored := make([]models.ScoredFile, 64)
	for i := range scored {
		scored[i] = models.ScoredFile{
			Record: models.FileRecord{
				Path:    fmt.Sprintf("/repo/file%02d.go", i),
				RelPath: fmt.Sprintf("file%02d.go", i),
			},
			Score: float64((i*7)%17) / 2,
			Reasons: []models.InclusionReason{{
				Signal: "base",
				Detail: fmt.Sprintf("%d", i),
				Weight: float64(i),
			}},
		}
	}
	// Duplicate absolute paths are legal inputs while indexes are being
	// merged. The optimized lookup must update every matching record.
	scored[63].Record.Path = scored[3].Record.Path

	relations := make([]models.FileRelation, 0, 400)
	for i := 0; i < 400; i++ {
		relations = append(relations, models.FileRelation{
			SourcePath: scored[i%len(scored)].Record.Path,
			TargetPath: scored[(i*11+3)%len(scored)].Record.Path,
			RelType:    "import",
		})
	}

	got := cloneScoredFiles(scored)
	want := cloneScoredFiles(scored)
	expandByGraphRelations(got, relations)
	naiveExpandByGraphRelations(want, relations)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("indexed graph expansion differs from the original scan:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestImportExpansionBasesEquivalentToBoundaryChecks(t *testing.T) {
	imports := []string{
		"",
		"foo",
		"pkg/foo",
		"foo.js",
		"foo.bar.js",
		"pkg/foo.js",
		"pkg/foo.bar.js",
		"@scope/pkg",
		"a//foo.js",
		"/",
		".hidden",
	}
	bases := []string{
		"",
		"foo",
		"foo.bar",
		"foo.js",
		"pkg",
		"bar",
		"@scope",
		"futility",
		"auth_utilities",
		".hidden",
	}

	for _, imp := range imports {
		got := make(map[string]struct{})
		for _, base := range importExpansionBases(imp) {
			got[base] = struct{}{}
		}
		for _, base := range bases {
			want := imp == base ||
				strings.HasSuffix(imp, "/"+base) ||
				strings.HasPrefix(imp, base+".") ||
				strings.Contains(imp, "/"+base+".")
			_, found := got[base]
			if found != want {
				t.Errorf("import %q, base %q: extracted=%v, predicate=%v (all=%v)", imp, base, found, want, got)
			}
		}
	}
}

func TestExpandByImportsMatchesNaiveScanScores(t *testing.T) {
	scored := []models.ScoredFile{
		{Record: models.FileRecord{RelPath: "src/main.ts", Imports: []string{"pkg/foo", "bar.js"}}, Score: 10},
		{Record: models.FileRecord{RelPath: "src/worker.ts", Imports: []string{"foo.bar.js"}}, Score: 9},
		{Record: models.FileRecord{RelPath: "src/foo.ts"}, Score: 1},
		{Record: models.FileRecord{RelPath: "other/foo.go"}, Score: 1},
		{Record: models.FileRecord{RelPath: "src/foo.bar.ts"}, Score: 1},
		{Record: models.FileRecord{RelPath: "src/bar.ts"}, Score: 1},
		{Record: models.FileRecord{RelPath: "src/futility.ts"}, Score: 1},
		{Record: models.FileRecord{RelPath: "src/auth_utilities.ts"}, Score: 1},
	}
	weights := DefaultWeights()
	got := cloneScoredFiles(scored)
	want := cloneScoredFiles(scored)

	expandByImports(got, nil, nil, &weights)
	naiveExpandByImports(want, &weights)

	for i := range want {
		if got[i].Score != want[i].Score {
			t.Errorf("%s: indexed score %v, naive score %v", got[i].Record.RelPath, got[i].Score, want[i].Score)
		}
	}
}

func TestResolveAliasChoosesMostSpecificPrefix(t *testing.T) {
	info := &project.Info{PathAliases: map[string]string{
		"@app":       "src",
		"@app/admin": "src/privileged",
		"@":          "fallback",
	}}
	aliases := orderedPathAliases(info)

	for run := 0; run < 20; run++ {
		if got, want := resolveAlias("@app/admin/user", aliases), "src/privileged/user"; got != want {
			t.Fatalf("run %d: resolveAlias() = %q, want %q", run, got, want)
		}
	}
}

func TestDependencyMatchIsStableWhenSeveralDependenciesMatch(t *testing.T) {
	scored := []models.ScoredFile{{
		Record: models.FileRecord{Imports: []string{"z-auth", "a-auth", "a-auth"}},
	}}
	weights := DefaultWeights()

	applyDependencyMatches(scored, []string{"auth"}, []string{"z-auth", "a-auth", "a-auth"}, &weights)

	if got := scored[0].Score; got != weights.DependencyMatch {
		t.Fatalf("dependency score = %v, want one bonus %v", got, weights.DependencyMatch)
	}
	if len(scored[0].Reasons) != 1 || scored[0].Reasons[0].Detail != "a-auth" {
		t.Fatalf("dependency explanation is not deterministic: %#v", scored[0].Reasons)
	}
}

func BenchmarkExpandByGraphRelationsIndexed(b *testing.B) {
	const (
		fileCount     = 4_000
		relationCount = 20_000
	)
	base := make([]models.ScoredFile, fileCount)
	for i := range base {
		base[i] = models.ScoredFile{
			Record: models.FileRecord{
				Path:    fmt.Sprintf("/repo/file%05d.go", i),
				RelPath: fmt.Sprintf("file%05d.go", i),
			},
			Score: float64(i % 20),
		}
	}
	relations := make([]models.FileRelation, relationCount)
	for i := range relations {
		relations[i] = models.FileRelation{
			SourcePath: base[i%fileCount].Record.Path,
			TargetPath: base[(i*37+11)%fileCount].Record.Path,
			RelType:    "import",
		}
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		scored := cloneScoredFiles(base)
		expandByGraphRelations(scored, relations)
	}
}

func cloneScoredFiles(files []models.ScoredFile) []models.ScoredFile {
	clone := append([]models.ScoredFile(nil), files...)
	for i := range clone {
		clone[i].Reasons = append([]models.InclusionReason(nil), files[i].Reasons...)
	}
	return clone
}

// naiveExpandByImports is the previous candidate×import scan. It deliberately
// compares only scores in the test because the old map iteration made the
// explanation detail unstable when several imports matched one file.
func naiveExpandByImports(scored []models.ScoredFile, weights *Weights) {
	tmp := append([]models.ScoredFile(nil), scored...)
	sort.Slice(tmp, func(i, j int) bool { return tmp[i].Score > tmp[j].Score })
	limit := expansionLimit
	if limit > len(tmp) {
		limit = len(tmp)
	}
	importedPaths := make(map[string]bool)
	for _, seed := range tmp[:limit] {
		for _, fileImport := range seed.Record.Imports {
			importedPaths[strings.ToLower(fileImport)] = true
		}
	}
	for i := range scored {
		base := strings.ToLower(stripExt(filepath.Base(scored[i].Record.RelPath)))
		for fileImport := range importedPaths {
			if fileImport != base &&
				!strings.HasSuffix(fileImport, "/"+base) &&
				!strings.HasPrefix(fileImport, base+".") &&
				!strings.Contains(fileImport, "/"+base+".") {
				continue
			}
			scored[i].Score += weights.ImportExpansion
			break
		}
	}
}

// naiveExpandByGraphRelations is the pre-index implementation retained in the
// test as an executable equivalence specification.
func naiveExpandByGraphRelations(scored []models.ScoredFile, relations []models.FileRelation) {
	if len(relations) == 0 {
		return
	}
	tmp := append([]models.ScoredFile(nil), scored...)
	sort.Slice(tmp, func(i, j int) bool {
		if math.Abs(tmp[i].Score-tmp[j].Score) < 1e-9 {
			return tmp[i].Record.RelPath < tmp[j].Record.RelPath
		}
		return tmp[i].Score > tmp[j].Score
	})
	limit := expansionLimit
	if limit > len(tmp) {
		limit = len(tmp)
	}
	seeds := make(map[string]bool)
	for i := 0; i < limit; i++ {
		if tmp[i].Score >= 1 {
			seeds[tmp[i].Record.Path] = true
		}
	}
	for _, relation := range relations {
		if seeds[relation.SourcePath] {
			for i := range scored {
				if scored[i].Record.Path != relation.TargetPath {
					continue
				}
				scored[i].Score += 1
				scored[i].Reasons = append(scored[i].Reasons, models.InclusionReason{
					Signal: "dependency_relation",
					Detail: "imported by top file",
					Weight: 1,
				})
			}
		}
		if seeds[relation.TargetPath] {
			for i := range scored {
				if scored[i].Record.Path != relation.SourcePath {
					continue
				}
				scored[i].Score += 0.8
				scored[i].Reasons = append(scored[i].Reasons, models.InclusionReason{
					Signal: "consumer_relation",
					Detail: "imports top file",
					Weight: 0.8,
				})
			}
		}
	}
}

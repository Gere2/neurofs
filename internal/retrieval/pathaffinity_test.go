package retrieval

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/storage"
)

func affinityCandidate(relPath string, score float64) candidate {
	return candidate{
		hit:      Hit{Path: relPath, Score: score, ContentHash: relPath},
		filePath: "/repo/" + relPath,
	}
}

func TestDominantPathPrefix(t *testing.T) {
	cases := map[string]string{
		"apps/brain/lib/inventory/consumption/job-contract.ts": "apps/brain/lib/inventory",
		"apps/brain/lib/gmail/oauth.ts":                        "apps/brain/lib",
		"internal/retrieval/search.go":                         "internal/retrieval",
		"main.go":                                              "",
		"":                                                     "",
	}
	for in, want := range cases {
		if got := dominantPathPrefix(in); got != want {
			t.Errorf("dominantPathPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPathWithinPrefix(t *testing.T) {
	if !pathWithinPrefix("apps/brain/lib/inventory/consumption/job-contract.ts", "apps/brain/lib/inventory") {
		t.Error("a file under the prefix must be inside it")
	}
	if pathWithinPrefix("apps/brain/lib/inventoryx/other.ts", "apps/brain/lib/inventory") {
		t.Error("prefix matching must respect path segment boundaries")
	}
	if !pathWithinPrefix("apps/brain/lib/inventory", "apps/brain/lib/inventory") {
		t.Error("the prefix itself must count as inside")
	}
}

// TestPathAffinityDownranksUnrelatedDirectory is the raiz-app case: a
// consumption-contract query whose top hit dominates still dragged
// gmail/oauth.ts into the bundle on the word "identity".
func TestPathAffinityDownranksUnrelatedDirectory(t *testing.T) {
	w := defaultTestWeights()
	candidates := []candidate{
		affinityCandidate("apps/brain/lib/inventory/consumption/job-contract.ts", 110),
		affinityCandidate("apps/brain/lib/inventory/consumption/job-transitions.ts", 64),
		affinityCandidate("apps/brain/lib/inventory/adapters/movement.ts", 40),
		affinityCandidate("apps/brain/lib/gmail/oauth.ts", 21),
		affinityCandidate("apps/pos/scripts/seed.ts", 18),
	}

	applyPathAffinity(candidates, nil, w)

	if got := candidates[0].hit.Score; got != 110 {
		t.Errorf("top hit must not be penalised, got %v", got)
	}
	if got := candidates[1].hit.Score; got != 64 {
		t.Errorf("sibling in the dominant path must not be penalised, got %v", got)
	}
	if got := candidates[2].hit.Score; got != 40 {
		t.Errorf("sibling module under the dominant path must not be penalised, got %v", got)
	}
	wantOAuth := 21 * w.PathAffinityKeep
	if got := candidates[3].hit.Score; got != wantOAuth {
		t.Errorf("gmail/oauth.ts score = %v, want %v", got, wantOAuth)
	}
	if !containsString(candidates[3].hit.Reasons, "path_affinity_downrank") {
		t.Errorf("expected path_affinity_downrank reason, got %v", candidates[3].hit.Reasons)
	}
	if got := candidates[4].hit.Score; got != 18*w.PathAffinityKeep {
		t.Errorf("apps/pos/scripts/seed.ts score = %v, want %v", got, 18*w.PathAffinityKeep)
	}

	// The bundle is cut by score: after the penalty the unrelated hits fall
	// below the in-path results they were displacing.
	ranked := rankedCandidateOrder(candidates)
	for _, i := range ranked[:3] {
		if !pathWithinPrefix(candidates[i].hit.Path, "apps/brain/lib/inventory") {
			t.Fatalf("top-3 after affinity leaked %q", candidates[i].hit.Path)
		}
	}
	if candidates[3].hit.Score >= candidates[2].hit.Score {
		t.Fatalf("gmail/oauth.ts (%v) must rank below the in-path hit (%v)",
			candidates[3].hit.Score, candidates[2].hit.Score)
	}
}

func TestPathAffinityExemptsFilesImportedByTopHit(t *testing.T) {
	w := defaultTestWeights()
	candidates := []candidate{
		affinityCandidate("apps/brain/lib/inventory/consumption/job-contract.ts", 110),
		affinityCandidate("apps/brain/lib/inventory/consumption/types.ts", 64),
		affinityCandidate("apps/brain/lib/inventory/adapters/movement.ts", 40),
		affinityCandidate("packages/shared/src/org.ts", 30),
		affinityCandidate("apps/brain/lib/gmail/oauth.ts", 21),
	}
	relations := []models.FileRelation{{
		SourcePath: "/repo/apps/brain/lib/inventory/consumption/job-contract.ts",
		TargetPath: "/repo/packages/shared/src/org.ts",
		RelType:    "import",
	}}

	applyPathAffinity(candidates, relations, w)

	if got := candidates[3].hit.Score; got != 30 {
		t.Errorf("a file imported by the top hit must keep its score, got %v", got)
	}
	if !containsString(candidates[3].hit.Reasons, "path_affinity_import_exempt") {
		t.Errorf("expected the exemption to be recorded, got %v", candidates[3].hit.Reasons)
	}
	if got := candidates[4].hit.Score; got != 21*w.PathAffinityKeep {
		t.Errorf("an unimported outsider must still be penalised, got %v", got)
	}
}

func TestPathAffinityInertWithoutADominantHit(t *testing.T) {
	w := defaultTestWeights()
	candidates := []candidate{
		affinityCandidate("apps/brain/lib/inventory/consumption/job-contract.ts", 30),
		affinityCandidate("apps/brain/lib/gmail/oauth.ts", 26),
		affinityCandidate("apps/pos/scripts/seed.ts", 22),
	}

	applyPathAffinity(candidates, nil, w)

	for _, c := range candidates {
		if containsString(c.hit.Reasons, "path_affinity_downrank") {
			t.Fatalf("no dominant hit (30 < 1.5 x 22) must leave every score untouched: %+v", c.hit)
		}
	}
	if candidates[1].hit.Score != 26 || candidates[2].hit.Score != 22 {
		t.Fatalf("scores changed without a dominant hit: %v, %v", candidates[1].hit.Score, candidates[2].hit.Score)
	}
}

func TestPathAffinityInertForRootLevelTopHit(t *testing.T) {
	w := defaultTestWeights()
	candidates := []candidate{
		affinityCandidate("main.go", 110),
		affinityCandidate("internal/foo/a.go", 30),
		affinityCandidate("internal/bar/b.go", 20),
	}

	applyPathAffinity(candidates, nil, w)

	if candidates[1].hit.Score != 30 || candidates[2].hit.Score != 20 {
		t.Fatalf("a root-level winner names no feature area: %v, %v",
			candidates[1].hit.Score, candidates[2].hit.Score)
	}
}

func TestPathAffinityInertWhenWeightIsNeutral(t *testing.T) {
	w := defaultTestWeights()
	w.PathAffinityKeep = 1.0
	candidates := []candidate{
		affinityCandidate("apps/brain/lib/inventory/consumption/job-contract.ts", 110),
		affinityCandidate("apps/brain/lib/inventory/consumption/types.ts", 64),
		affinityCandidate("apps/brain/lib/gmail/oauth.ts", 21),
	}

	applyPathAffinity(candidates, nil, w)

	if candidates[2].hit.Score != 21 {
		t.Fatalf("keep=1.0 must be inert, got %v", candidates[2].hit.Score)
	}
	if len(candidates[2].hit.Reasons) != 0 {
		t.Fatalf("keep=1.0 must not annotate hits, got %v", candidates[2].hit.Reasons)
	}
}

// TestPathAffinityDominanceIsJudgedBetweenFiles pins the reason dominance
// counts files rather than chunks. The winning file owns the first three
// chunk slots here, so a chunk-rank probe compares it against itself
// (52.50 vs 35.75 = 1.47) and path affinity silently never fires — which is
// exactly how it shipped until an end-to-end probe caught it.
func TestPathAffinityDominanceIsJudgedBetweenFiles(t *testing.T) {
	w := defaultTestWeights()
	candidates := []candidate{
		affinityCandidate("apps/brain/lib/inventory/consumption/job-contract.ts", 52.5),
		affinityCandidate("apps/brain/lib/inventory/consumption/job-contract.ts", 39.75),
		affinityCandidate("apps/brain/lib/inventory/consumption/job-contract.ts", 35.75),
		affinityCandidate("apps/brain/lib/inventory/consumption/job-transitions.ts", 23.75),
		affinityCandidate("apps/brain/lib/gmail/oauth.ts", 20.25),
	}
	// affinityCandidate keys filePath off the rel path, so the three chunks
	// of one file must share it the way a real search would.
	for i := 0; i < 3; i++ {
		candidates[i].filePath = "/repo/apps/brain/lib/inventory/consumption/job-contract.ts"
	}

	if order := rankedFileOrder(candidates); len(order) != 3 {
		t.Fatalf("expected 3 distinct files in the probe order, got %d", len(order))
	}

	applyPathAffinity(candidates, nil, w)

	if got, want := candidates[4].hit.Score, 20.25*w.PathAffinityKeep; got != want {
		t.Fatalf("gmail/oauth.ts = %v, want %v — dominance must be measured against the runner-up FILE (20.25), not the winner's own third chunk (35.75)", got, want)
	}
	for i := 0; i < 4; i++ {
		if containsString(candidates[i].hit.Reasons, "path_affinity_downrank") {
			t.Errorf("in-path candidate %d was penalised", i)
		}
	}
}

// TestPathAffinityThroughRealSearch exercises the whole pipeline — index,
// score, penalise, rank — on a monorepo-shaped fixture, because every
// unit-level assertion above would still pass with the feature inert.
func TestPathAffinityThroughRealSearch(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		full := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
	}
	write("apps/brain/lib/inventory/consumption/job-contract.ts", `
export type JobContractIdentity = { orgId: string };
export function fulfillmentIdentity(job: JobContractIdentity): string {
  return job.orgId;
}
export function classifyConsumptionJobDocument(doc: string): string {
  return doc;
}
`)
	write("apps/brain/lib/inventory/consumption/job-transitions.ts", `
export const JOB_TRANSITIONS = ["pending", "done"];
export function nextJobTransition(state: string): string {
  return state;
}
`)
	write("apps/brain/lib/gmail/oauth.ts", `
export type GoogleIdentity = { orgId: string };
export function googleIdentityToken(identity: GoogleIdentity): string {
  return identity.orgId;
}
`)
	write("apps/pos/scripts/seed.ts", `
export function seedIdentity(orgId: string): string {
  return orgId;
}
`)

	cfg, err := config.New(repo)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("storage: %v", err)
	}
	if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
		_ = db.Close()
		t.Fatalf("indexer.Run: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close index: %v", err)
	}

	resp, err := Search(context.Background(), Options{
		Query: "fulfillmentIdentity job contract",
		Repo:  repo,
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(resp.Results) == 0 {
		t.Fatal("expected results")
	}

	var penalised int
	for _, hit := range resp.Results {
		inPath := pathWithinPrefix(hit.Path, "apps/brain/lib/inventory")
		down := containsString(hit.Reasons, "path_affinity_downrank")
		if inPath && down {
			t.Errorf("%s is inside the dominant path but was penalised", hit.Path)
		}
		if !inPath && !down {
			t.Errorf("%s is outside the dominant path but escaped the penalty", hit.Path)
		}
		if down {
			penalised++
		}
	}
	if penalised == 0 {
		t.Fatal("path affinity never engaged through the real search pipeline")
	}
	// The point of the penalty: the unrelated directories end up behind
	// every in-path result rather than spending bundle budget beside them.
	for i, hit := range resp.Results {
		if !pathWithinPrefix(hit.Path, "apps/brain/lib/inventory") {
			for _, later := range resp.Results[i:] {
				if pathWithinPrefix(later.Path, "apps/brain/lib/inventory") {
					t.Errorf("out-of-path %s outranked in-path %s", hit.Path, later.Path)
				}
			}
			break
		}
	}
}

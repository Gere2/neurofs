package taskflow

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gere2/neurofs/internal/audit"
	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/memory"
	"github.com/Gere2/neurofs/internal/models"
)

// TestEnrichBundle_PopulatesIdentityFields verifies the audit-identity
// fields a compliance consumer needs are present after enrichment:
// resolved repo path, generation timestamp, and a content hash. CommitSHA
// is checked in TestEnrichBundle_CommitSHAFromGit; here we tolerate an
// empty SHA because the test tempdir is not a git worktree.
func TestEnrichBundle_PopulatesIdentityFields(t *testing.T) {
	repo := t.TempDir()
	before := time.Now().UTC().Add(-time.Second)

	b := EnrichBundle(models.Bundle{
		Query:     "q",
		Fragments: []models.ContextFragment{{RelPath: "a.go", Content: "x"}},
	}, repo)

	if b.Repo == "" || !filepath.IsAbs(b.Repo) {
		t.Errorf("Repo must be absolute; got %q", b.Repo)
	}
	if b.GeneratedAt.Before(before) {
		t.Errorf("GeneratedAt must be set to now; got %v", b.GeneratedAt)
	}
	if len(b.BundleHash) != 64 {
		t.Errorf("BundleHash must be sha256-hex (64 chars); got %d chars", len(b.BundleHash))
	}
	if b.HashAlgorithm != audit.BundleHashAlgorithm {
		t.Errorf("HashAlgorithm = %q, want %q", b.HashAlgorithm, audit.BundleHashAlgorithm)
	}
}

// TestEnrichBundle_HashStableAcrossEnrichmentRuns confirms BundleHash
// excludes GeneratedAt — otherwise two enrich runs with identical
// content would produce different hashes, defeating the "same context"
// guarantee.
func TestEnrichBundle_HashStableAcrossEnrichmentRuns(t *testing.T) {
	repo := t.TempDir()
	b := models.Bundle{
		Query:     "q",
		Fragments: []models.ContextFragment{{RelPath: "a.go", Content: "x"}},
	}
	h1 := EnrichBundle(b, repo).BundleHash
	time.Sleep(10 * time.Millisecond)
	h2 := EnrichBundle(b, repo).BundleHash
	if h1 != h2 {
		t.Errorf("BundleHash must NOT depend on GeneratedAt; got %s vs %s", h1, h2)
	}
}

// TestEnrichBundle_CommitSHAFromGit confirms we capture the HEAD commit
// when the repo is a git worktree.
func TestEnrichBundle_CommitSHAFromGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	b := EnrichBundle(models.Bundle{Query: "q"}, repo)
	if len(b.CommitSHA) != 40 {
		t.Errorf("CommitSHA must be a 40-char hex SHA from git rev-parse; got %q", b.CommitSHA)
	}
}

// TestTopPicks pins the structured shape of the top-N selection that
// both the CLI summary and the UI panel render. If fields move or the
// ordering contract changes, the UX affordance ("see what landed
// without opening the prompt") regresses silently.
func TestTopPicks(t *testing.T) {
	t.Parallel()

	b := models.Bundle{
		Fragments: []models.ContextFragment{
			{RelPath: "internal/ranking/ranking.go", Tokens: 820, Representation: models.Representation("full_code"), Score: 9.1},
			{RelPath: "internal/packager/packager.go", Tokens: 410, Representation: models.Representation("signature"), Score: 6.4},
			{RelPath: "cmd/neurofs/main.go", Tokens: 90, Representation: models.Representation("full_code"), Score: 2.1},
		},
	}

	t.Run("respects n and fragment order", func(t *testing.T) {
		got := TopPicks(b, 2)
		if len(got) != 2 {
			t.Fatalf("want 2 picks, got %d: %+v", len(got), got)
		}
		if got[0].RelPath != "internal/ranking/ranking.go" {
			t.Fatalf("first pick wrong: %+v", got[0])
		}
		if got[0].Tokens != 820 || got[0].Representation != "full_code" || got[0].Score != 9.1 {
			t.Fatalf("first pick fields wrong: %+v", got[0])
		}
	})

	t.Run("caps at fragment count", func(t *testing.T) {
		got := TopPicks(b, 99)
		if len(got) != 3 {
			t.Fatalf("want 3 picks, got %d", len(got))
		}
	})

	t.Run("nil on empty or zero", func(t *testing.T) {
		if got := TopPicks(b, 0); got != nil {
			t.Fatalf("want nil for n=0, got %v", got)
		}
		if got := TopPicks(models.Bundle{}, 5); got != nil {
			t.Fatalf("want nil for empty bundle, got %v", got)
		}
	})
}

// TestBaseName guarantees cache-key determinism: same inputs → same
// filename; a different budget must yield a different filename.
func TestBaseName(t *testing.T) {
	t.Parallel()

	a := BaseName("implement resume from record", 8000)
	b := BaseName("implement resume from record", 8000)
	if a != b {
		t.Fatalf("same inputs must produce same base name: %q vs %q", a, b)
	}
	c := BaseName("implement resume from record", 3000)
	if a == c {
		t.Fatalf("different budget must produce different base: both %q", a)
	}
	if len(a) < 18 || a[16] != '-' {
		t.Fatalf("base name shape wrong: %q", a)
	}
}

func TestCacheManifestDetectsTamperingAndRevisionChanges(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "task.prompt.txt")
	bundlePath := filepath.Join(dir, "task.bundle.json")
	manifestPath := filepath.Join(dir, "task.manifest.json")
	bundle := EnrichBundle(models.Bundle{
		Query: "q",
		Fragments: []models.ContextFragment{{
			RelPath: "a.go", Lang: models.LangGo,
			Representation: models.RepFullCode, Content: "package a",
		}},
	}, dir)
	prompt := []byte("prompt")
	bundle.RenderedPromptHash = sha256Hex(prompt)
	bundle.RenderedPromptHashAlgorithm = renderedPromptHashAlgorithm
	bundleBytes, err := json.Marshal(bundle)
	if err != nil {
		t.Fatal(err)
	}
	manifest := cacheManifest{
		Version: cacheManifestVersion, Revision: "rev-a",
		PromptSHA256: sha256Hex(prompt),
		BundleSHA256: sha256Hex(bundleBytes),
		BundleHash:   bundle.BundleHash,
	}
	manifestBytes, _ := json.Marshal(manifest)
	for path, data := range map[string][]byte{
		promptPath: prompt, bundlePath: bundleBytes, manifestPath: manifestBytes,
	} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !isCacheFreshWithManifest(promptPath, bundlePath, manifestPath, "rev-a") {
		t.Fatal("valid manifest should reuse cache")
	}
	if isCacheFreshWithManifest(promptPath, bundlePath, manifestPath, "rev-b") {
		t.Fatal("changed generation inputs must invalidate cache")
	}
	if err := os.WriteFile(promptPath, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if isCacheFreshWithManifest(promptPath, bundlePath, manifestPath, "rev-a") {
		t.Fatal("partial or tampered prompt must invalidate cache")
	}
}

func TestExecutableIdentityIsStableAndHashed(t *testing.T) {
	first := executableIdentity()
	second := executableIdentity()
	if first != second {
		t.Fatalf("executable identity changed within one process: %q vs %q", first, second)
	}
	if len(first) != sha256.Size*2 {
		t.Fatalf("executable identity = %q, want a SHA-256 hex digest", first)
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Fatalf("executable identity is not hex: %v", err)
	}
}

func TestCacheAndBundleReadsRejectUnsafeArtifacts(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.json")
	if err := os.WriteFile(outside, []byte(`{"query":"outside"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked.bundle.json")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := readBundleJSON(link); err == nil {
		t.Fatal("readBundleJSON followed a symlink")
	}

	manifestPath := filepath.Join(dir, "oversized.manifest.json")
	if err := os.WriteFile(manifestPath, make([]byte, maxCacheMetadataBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if isCacheFreshWithManifest(
		filepath.Join(dir, "prompt.txt"),
		filepath.Join(dir, "bundle.json"),
		manifestPath,
		"revision",
	) {
		t.Fatal("oversized cache manifest was accepted")
	}

	cfg := &config.Config{
		RepoRoot: dir,
		DBPath:   filepath.Join(dir, config.DirName, config.DBName),
		Budget:   config.DefaultBudget,
	}
	if err := os.MkdirAll(cfg.DBDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(cfg.DBDir(), "config.json")
	if err := os.Symlink(outside, configPath); err != nil {
		t.Fatal(err)
	}
	if _, err := cacheRevision(cfg, true, false); err == nil {
		t.Fatal("cache revision followed a symlinked mutable input")
	}
}

func TestCacheRevisionTracksCompleteUntrackedFileContent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	initGitRepo(t, repo)

	path := filepath.Join(repo, "notes.txt")
	if err := os.WriteFile(path, []byte("alpha"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := cacheRevisionTestConfig(repo)
	before, err := cacheRevision(cfg, true, false)
	if err != nil {
		t.Fatalf("initial cache revision: %v", err)
	}

	// Keep the path, status and byte length identical. Only a complete content
	// fingerprint can distinguish these two untracked generations.
	if err := os.WriteFile(path, []byte("bravo"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := cacheRevision(cfg, true, false)
	if err != nil {
		t.Fatalf("updated cache revision: %v", err)
	}
	if before == after {
		t.Fatal("untracked content change did not invalidate the cache revision")
	}
}

func TestCacheRevisionTracksChangesBeyondPromptDiffPrefix(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	initGitRepo(t, repo)

	largePath := filepath.Join(repo, "a-large.txt")
	tailPath := filepath.Join(repo, "z-tail.txt")
	if err := os.WriteFile(largePath, []byte(strings.Repeat("before\n", 2000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tailPath, []byte("baseline"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-qm", "baseline")

	if err := os.WriteFile(largePath, []byte(strings.Repeat("after\n", 2000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tailPath, []byte("first__"), 0o644); err != nil {
		t.Fatal(err)
	}
	statusBefore := GitStatus(repo)
	diffBefore := GitDiff(repo)
	cfg := cacheRevisionTestConfig(repo)
	before, err := cacheRevision(cfg, true, false)
	if err != nil {
		t.Fatalf("initial cache revision: %v", err)
	}

	if err := os.WriteFile(tailPath, []byte("second_"), 0o644); err != nil {
		t.Fatal(err)
	}
	statusAfter := GitStatus(repo)
	diffAfter := GitDiff(repo)
	if statusBefore != statusAfter {
		t.Fatalf("test setup changed porcelain status:\nbefore=%q\nafter=%q", statusBefore, statusAfter)
	}
	if diffBefore != diffAfter {
		t.Fatal("test setup did not place the tail edit beyond GitDiff's truncated prefix")
	}
	after, err := cacheRevision(cfg, true, false)
	if err != nil {
		t.Fatalf("updated cache revision: %v", err)
	}
	if before == after {
		t.Fatal("change beyond the prompt diff prefix did not invalidate cache revision")
	}
}

func cacheRevisionTestConfig(repo string) *config.Config {
	return &config.Config{
		RepoRoot: repo,
		DBPath:   filepath.Join(repo, config.DirName, config.DBName),
		Budget:   config.DefaultBudget,
	}
}

func initGitRepo(t *testing.T, repo string) {
	t.Helper()
	runGit(t, repo, "init", "-q")
	runGit(t, repo, "config", "user.email", "t@t")
	runGit(t, repo, "config", "user.name", "t")
	runGit(t, repo, "commit", "--allow-empty", "-qm", "initial")
}

func runGit(t *testing.T, repo string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

// TestSlugify covers lowercase, non-alnum collapse, the 40-char cap
// with hyphen trim, and the empty-string fallback. The fallback case
// matters — a slug that collapses to "" would mean every query
// shared one cache slot.
func TestSlugify(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in, want string
	}{
		{"Hello World!", "hello-world"},
		{"  leading/trailing  ", "leading-trailing"},
		{"multi   space --- dashes", "multi-space-dashes"},
		{"", "task"},
		{"!!!", "task"},
		{strings.Repeat("a", 60), strings.Repeat("a", 40)},
	}
	for _, tc := range cases {
		if got := Slugify(tc.in); got != tc.want {
			t.Fatalf("Slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRunUsesChunkMode(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")

	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module example.com/chunktest\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	source := `package chunktest

func BuildThing(name string) string {
	return "build:" + name
}

func OtherThing(name string) string {
	return "other:" + name
}
`
	if err := os.WriteFile(filepath.Join(tmp, "builder.go"), []byte(source), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	result, err := Run(Opts{
		RepoRoot:      tmp,
		Query:         "Where is BuildThing implemented?",
		Budget:        1200,
		Force:         true,
		DisableChunks: false,
	})
	if err != nil {
		t.Fatalf("Run chunk mode failed: %v", err)
	}
	if !result.ChunkMode {
		t.Fatalf("expected ChunkMode=true")
	}
	if len(result.Bundle.Fragments) == 0 {
		t.Fatalf("expected chunk fragments")
	}
	frag := result.Bundle.Fragments[0]
	if frag.Representation != models.RepExcerpt {
		t.Fatalf("expected excerpt fragment, got %q", frag.Representation)
	}
	if !strings.Contains(frag.Content, "// lines:") || !strings.Contains(frag.Content, "BuildThing") {
		t.Fatalf("fragment does not look like a chunk excerpt:\n%s", frag.Content)
	}
	if !strings.Contains(result.Prompt, `rep="excerpt"`) || !strings.Contains(result.Prompt, "// lines:") {
		t.Fatalf("prompt missing excerpt metadata:\n%s", result.Prompt)
	}
	if got, want := result.Bundle.RenderedPromptHash, sha256Hex([]byte(result.Prompt)); got != want {
		t.Fatalf("RenderedPromptHash = %q, want exact prompt hash %q", got, want)
	}
	if got := result.Bundle.RenderedPromptHashAlgorithm; got != renderedPromptHashAlgorithm {
		t.Fatalf("RenderedPromptHashAlgorithm = %q, want %q", got, renderedPromptHashAlgorithm)
	}
	if !strings.Contains(filepath.Base(result.PromptPath), "chunks-") {
		t.Fatalf("chunk cache should use a distinct filename, got %s", result.PromptPath)
	}
}

func TestGitDiffAndStatus(t *testing.T) {
	tmp := t.TempDir()
	diff := GitDiff(tmp)
	if diff != "" {
		t.Errorf("expected empty diff on non-git dir, got: %q", diff)
	}

	status := GitStatus(tmp)
	if status != "" {
		t.Errorf("expected empty status on non-git dir, got: %q", status)
	}
}

func TestGitDiffFiltersSensitiveAndIgnoredFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	files := map[string]string{
		"main.go":             "package main\nconst visible = \"before\"\n",
		".env":                "TOKEN=before\n",
		"config/secrets.yaml": "token: before\n",
		"ignored.go":          "package ignored\nconst value = \"before\"\n",
		".neurofsignore":      "ignored.go\n",
	}
	for name, content := range files {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"config", "user.email", "t@t"},
		{"config", "user.name", "t"},
		{"add", "."},
		{"commit", "-qm", "initial"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	updates := map[string]string{
		"main.go":             "package main\nconst visible = \"after\"\n",
		".env":                "TOKEN=do-not-leak\n",
		"config/secrets.yaml": "token: do-not-leak\n",
		"ignored.go":          "package ignored\nconst value = \"do-not-leak\"\n",
	}
	for name, content := range updates {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	diff := GitDiff(repo)
	if !strings.Contains(diff, `visible = "after"`) {
		t.Fatalf("safe source diff missing:\n%s", diff)
	}
	status := GitStatus(repo)
	if !strings.Contains(status, "main.go") {
		t.Fatalf("safe source status missing:\n%s", status)
	}
	for _, secret := range []string{"TOKEN=", "do-not-leak", "secrets.yaml", "ignored.go"} {
		if strings.Contains(diff, secret) {
			t.Fatalf("sensitive or ignored content %q leaked into diff:\n%s", secret, diff)
		}
		if strings.Contains(status, secret) {
			t.Fatalf("sensitive or ignored path %q leaked into status:\n%s", secret, status)
		}
	}
}

func TestRunAutoLogsToLedger(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")

	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module example.com/chunktest\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	source := `package chunktest
func BuildThing(name string) string {
	return "build:" + name
}
`
	if err := os.WriteFile(filepath.Join(tmp, "builder.go"), []byte(source), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	store := memory.NewSqliteStore(tmp)
	// First Run (fresh generation)
	_, err := Run(Opts{
		RepoRoot:      tmp,
		Query:         "Where is BuildThing?",
		Budget:        1200,
		Force:         true,
		DisableChunks: false,
		Ledger:        memory.New(store),
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	// Read ledger
	entries, err := store.Read(context.Background(), "")
	if err != nil {
		t.Fatalf("failed to read ledger: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("expected 1 entry in ledger, got %d", len(entries))
	}

	if entries[0].Query != "Where is BuildThing?" {
		t.Errorf("expected query in ledger, got %s", entries[0].Query)
	}
	if !strings.Contains(entries[0].Notes, "fresh generation") {
		t.Errorf("expected fresh generation note in ledger, got %s", entries[0].Notes)
	}

	// Second Run (cache reused)
	_, err = Run(Opts{
		RepoRoot:      tmp,
		Query:         "Where is BuildThing?",
		Budget:        1200,
		Force:         false,
		DisableChunks: false,
		Ledger:        memory.New(store),
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	entries2, err := store.Read(context.Background(), "")
	if err != nil {
		t.Fatalf("failed to read ledger second time: %v", err)
	}

	if len(entries2) != 2 {
		t.Fatalf("expected 2 entries in ledger, got %d", len(entries2))
	}
	if !strings.Contains(entries2[1].Notes, "cache reused") {
		t.Errorf("expected cache reused note in second ledger entry, got %s", entries2[1].Notes)
	}
}

func TestEnsureFreshIndex(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")

	// Create a dummy go.mod so that the scanner has something to do and doesn't skip
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module example.com/freshindex\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("write main.go: %v", err)
	}

	dbPath := filepath.Join(tmp, "index.db")
	cfg := &config.Config{
		RepoRoot: tmp,
		DBPath:   dbPath,
		Budget:   8000,
	}

	// 1. Initial run: DB doesn't exist, EnsureFreshIndex must scan and create it.
	if _, err := os.Stat(dbPath); !os.IsNotExist(err) {
		t.Fatalf("db should not exist yet")
	}

	err := EnsureFreshIndex(cfg)
	if err != nil {
		t.Fatalf("EnsureFreshIndex failed: %v", err)
	}

	stat, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("db was not created: %v", err)
	}
	initialMtime := stat.ModTime()

	// 2. Second run: DB exists and is fresh, EnsureFreshIndex should do nothing (no re-scan).
	err = EnsureFreshIndex(cfg)
	if err != nil {
		t.Fatalf("EnsureFreshIndex failed on second run: %v", err)
	}

	stat, err = os.Stat(dbPath)
	if err != nil {
		t.Fatalf("db disappeared: %v", err)
	}
	if stat.ModTime() != initialMtime {
		t.Errorf("expected DB mtime to remain unchanged when fresh")
	}

	// 3. Stale index run: modify mtime of DB to be older than 24 hours. EnsureFreshIndex should re-scan.
	staleTime := time.Now().Add(-25 * time.Hour)
	if err := os.Chtimes(dbPath, staleTime, staleTime); err != nil {
		t.Fatalf("failed to change mtime: %v", err)
	}

	err = EnsureFreshIndex(cfg)
	if err != nil {
		t.Fatalf("EnsureFreshIndex failed on stale run: %v", err)
	}

	stat, err = os.Stat(dbPath)
	if err != nil {
		t.Fatalf("db disappeared: %v", err)
	}
	if stat.ModTime().Before(time.Now().Add(-1 * time.Minute)) {
		t.Errorf("expected DB to be re-scanned, but mtime remains stale: %v", stat.ModTime())
	}
}

func TestMissingRepoRootFailsWithoutSideEffects(t *testing.T) {
	parent := t.TempDir()
	missing := filepath.Join(parent, "missing-repo")
	cfg := &config.Config{
		RepoRoot: missing,
		DBPath:   filepath.Join(missing, config.DirName, config.DBName),
		Budget:   config.DefaultBudget,
	}

	if _, err := Run(Opts{RepoRoot: missing, Query: "where is auth?"}); err == nil {
		t.Fatal("Run must reject a missing repository root")
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("Run created state under missing repository: stat error = %v", err)
	}

	if err := EnsureFreshIndex(cfg); err == nil {
		t.Fatal("EnsureFreshIndex must reject a missing repository root")
	}
	if _, err := os.Lstat(missing); !os.IsNotExist(err) {
		t.Fatalf("EnsureFreshIndex created state under missing repository: stat error = %v", err)
	}
}

func TestRunDisableIndexRefreshDoesNotCreateMissingIndex(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(repo, "answer.go"),
		[]byte("package answer\n\nconst Answer = 42\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	_, err := Run(Opts{
		RepoRoot:            repo,
		Query:               "Where is Answer defined?",
		Budget:              1200,
		Force:               true,
		DisableIndexRefresh: true,
	})
	if err == nil {
		t.Fatal("read-only Run succeeded without an existing index")
	}
	dbPath := filepath.Join(repo, config.DirName, config.DBName)
	if _, statErr := os.Lstat(dbPath); !os.IsNotExist(statErr) {
		t.Fatalf("read-only Run created an index: %v", statErr)
	}
}

func TestRunNoChunksRefreshesSourceGeneration(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	sourcePath := filepath.Join(repo, "answer.go")
	firstSource := []byte("package answer\n\nfunc AlphaThing() string { return \"alpha\" }\n")
	secondSource := []byte("package answer\n\nfunc BravoThing() string { return \"bravo\" }\n")
	if len(firstSource) != len(secondSource) {
		t.Fatal("test sources must have equal length")
	}
	if err := os.WriteFile(sourcePath, firstSource, 0o644); err != nil {
		t.Fatal(err)
	}

	first, err := Run(Opts{
		RepoRoot:      repo,
		Query:         "Where is AlphaThing?",
		Budget:        1200,
		Force:         true,
		DisableChunks: true,
	})
	if err != nil {
		t.Fatalf("initial no-chunks run: %v", err)
	}
	if !strings.Contains(first.Prompt, "AlphaThing") {
		t.Fatalf("initial prompt does not contain AlphaThing:\n%s", first.Prompt)
	}
	cached, err := Run(Opts{
		RepoRoot:      repo,
		Query:         "Where is AlphaThing?",
		Budget:        1200,
		DisableChunks: true,
	})
	if err != nil {
		t.Fatalf("cached no-chunks run: %v", err)
	}
	if !cached.Reused {
		t.Fatal("freshness preflight invalidated an unchanged no-chunks cache")
	}
	info, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, secondSource, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sourcePath, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}

	second, err := Run(Opts{
		RepoRoot:      repo,
		Query:         "Where is BravoThing?",
		Budget:        1200,
		Force:         true,
		DisableChunks: true,
	})
	if err != nil {
		t.Fatalf("no-chunks run after source edit: %v", err)
	}
	if !second.AutoScanned {
		t.Fatal("source checksum drift did not report an implicit index refresh")
	}
	if !strings.Contains(second.Prompt, "BravoThing") ||
		strings.Contains(second.Prompt, "AlphaThing") {
		t.Fatalf("prompt did not use the refreshed source generation:\n%s", second.Prompt)
	}
}

func TestRunNoChunksReadOnlyRejectsStaleSourceWithoutWrites(t *testing.T) {
	repo := t.TempDir()
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	sourcePath := filepath.Join(repo, "answer.go")
	firstSource := []byte("package answer\n\nfunc AlphaThing() string { return \"alpha\" }\n")
	secondSource := []byte("package answer\n\nfunc BravoThing() string { return \"bravo\" }\n")
	if err := os.WriteFile(sourcePath, firstSource, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Run(Opts{
		RepoRoot:      repo,
		Query:         "Where is AlphaThing?",
		Budget:        1200,
		Force:         true,
		DisableChunks: true,
	}); err != nil {
		t.Fatalf("initial no-chunks run: %v", err)
	}
	cfg, err := config.New(repo)
	if err != nil {
		t.Fatal(err)
	}
	beforeDB, err := os.ReadFile(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeCache, err := os.ReadDir(filepath.Join(cfg.DBDir(), "task"))
	if err != nil {
		t.Fatal(err)
	}
	sourceInfo, err := os.Stat(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, secondSource, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sourcePath, sourceInfo.ModTime(), sourceInfo.ModTime()); err != nil {
		t.Fatal(err)
	}

	_, err = Run(Opts{
		RepoRoot:            repo,
		Query:               "Where is BravoThing?",
		Budget:              1200,
		Force:               true,
		DisableChunks:       true,
		DisableIndexRefresh: true,
	})
	if err == nil || !strings.Contains(err.Error(), "automatic refresh is disabled") {
		t.Fatalf("read-only stale run error = %v, want explicit refresh-disabled error", err)
	}
	afterDB, err := os.ReadFile(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(afterDB) != sha256.Sum256(beforeDB) ||
		afterInfo.Size() != beforeInfo.Size() ||
		!afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Fatal("read-only stale run changed the index database")
	}
	afterCache, err := os.ReadDir(filepath.Join(cfg.DBDir(), "task"))
	if err != nil {
		t.Fatal(err)
	}
	if len(afterCache) != len(beforeCache) {
		t.Fatalf("read-only stale run changed task cache entries: before=%d after=%d", len(beforeCache), len(afterCache))
	}
}

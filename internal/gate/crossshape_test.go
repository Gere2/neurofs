package gate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Gere2/neurofs/internal/abeval"
	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/fsutil"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/storage"
)

const crossShapeTestHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestEvaluateG5SkipsWithoutEvidence(t *testing.T) {
	got := EvaluateG5(nil)
	if got.Verdict != Skip {
		t.Fatalf("verdict = %s, want SKIP", got.Verdict)
	}
}

func TestEvaluateG5PassesAllRequiredShapes(t *testing.T) {
	evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
	evidence.Shapes[0].Economy.MeanTokenReduction = 0.25 - 5e-13
	evidence.Shapes[0].G3.MeanRecall = 0.8 - 5e-13

	got := EvaluateG5(&evidence)
	if got.Verdict != Pass {
		t.Fatalf("verdict = %s, want PASS; detail: %s", got.Verdict, got.Detail)
	}
	if got.Numbers["shapes"] != 3 {
		t.Fatalf("shape count = %v, want 3", got.Numbers["shapes"])
	}
}

func TestEvaluateG5WithIdentityRejectsStaleInputs(t *testing.T) {
	evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
	current := CrossShapeCurrentIdentity{
		SourceTreeSHA256: evidence.SourceTreeSHA256,
		WeightsSHA256:    evidence.WeightsSHA256,
		FixtureSets: map[string]string{
			CrossShapeKindGoService:          evidence.Shapes[0].FixtureSetSHA256,
			CrossShapeKindPythonLibrary:      evidence.Shapes[1].FixtureSetSHA256,
			CrossShapeKindTypeScriptFrontend: evidence.Shapes[2].FixtureSetSHA256,
		},
		FixtureCounts: map[string]int{
			CrossShapeKindGoService:          evidence.Shapes[0].G3.Fixtures,
			CrossShapeKindPythonLibrary:      evidence.Shapes[1].G3.Fixtures,
			CrossShapeKindTypeScriptFrontend: evidence.Shapes[2].G3.Fixtures,
		},
		PinnedCommits: map[string]string{
			CrossShapeKindPythonLibrary:      evidence.Shapes[1].CommitSHA,
			CrossShapeKindTypeScriptFrontend: evidence.Shapes[2].CommitSHA,
		},
	}
	if got := EvaluateG5WithIdentity(&evidence, current); got.Verdict != Pass {
		t.Fatalf("matching identity verdict = %s, want PASS: %s", got.Verdict, got.Detail)
	}

	current.SourceTreeSHA256 = strings.Repeat("f", 64)
	current.FixtureSets[CrossShapeKindPythonLibrary] = strings.Repeat("e", 64)
	current.FixtureCounts[CrossShapeKindTypeScriptFrontend]++
	current.PinnedCommits[CrossShapeKindPythonLibrary] = strings.Repeat("a", 40)
	got := EvaluateG5WithIdentity(&evidence, current)
	if got.Verdict != Fail ||
		!strings.Contains(got.Detail, "source tree") ||
		!strings.Contains(got.Detail, "python_library fixtures") ||
		!strings.Contains(got.Detail, "typescript_frontend fixture coverage") ||
		!strings.Contains(got.Detail, "python_library pinned commit") {
		t.Fatalf("stale identity verdict = %+v", got)
	}
}

func TestEvaluateG5WithIdentityRejectsHistoricalSchema(t *testing.T) {
	evidence := validCrossShapeEvidence(time.Now().UTC().Format(time.RFC3339))
	evidence.SchemaVersion = legacyCrossShapeSchemaVersion
	evidence.RunConfig = nil
	for i := range evidence.Shapes {
		evidence.Shapes[i].G2.ReportSHA256 = ""
		evidence.Shapes[i].G3.ReportSHA256 = ""
	}
	got := EvaluateG5WithIdentity(&evidence, CrossShapeCurrentIdentity{})
	if got.Verdict != Fail || !strings.Contains(got.Detail, "historical") {
		t.Fatalf("historical evidence verdict = %+v", got)
	}
}

func TestEvaluateG5ForRepoRejectsHistoricalSchema(t *testing.T) {
	evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
	evidence.SchemaVersion = legacyCrossShapeSchemaVersion
	evidence.RunConfig = nil
	for i := range evidence.Shapes {
		evidence.Shapes[i].G2.ReportSHA256 = ""
		evidence.Shapes[i].G3.ReportSHA256 = ""
	}
	got := EvaluateG5ForRepo(&evidence, t.TempDir())
	if got.Verdict != Fail || !strings.Contains(got.Detail, "historical") {
		t.Fatalf("historical evidence verdict = %+v", got)
	}
}

func TestEvaluateG5FailsStrictMissRateWithConcreteDetail(t *testing.T) {
	evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
	evidence.Shapes[1].Economy.MissRate = 1.0 / 3.0

	got := EvaluateG5(&evidence)
	if got.Verdict != Fail {
		t.Fatalf("verdict = %s, want FAIL", got.Verdict)
	}
	if !strings.Contains(got.Detail, "click (python_library) economy miss rate") {
		t.Fatalf("detail does not identify the failing shape and metric: %q", got.Detail)
	}
}

func TestEvaluateG5FailsMissingRequiredShape(t *testing.T) {
	evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
	evidence.Shapes = evidence.Shapes[:2]

	got := EvaluateG5(&evidence)
	if got.Verdict != Fail {
		t.Fatalf("verdict = %s, want FAIL", got.Verdict)
	}
	if !strings.Contains(got.Detail, "missing required shapes: typescript_frontend") {
		t.Fatalf("detail = %q", got.Detail)
	}
}

func TestEvaluateG5RejectsDuplicateKindsAndBadHashes(t *testing.T) {
	t.Run("duplicate kind", func(t *testing.T) {
		evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
		evidence.Shapes[1].Kind = evidence.Shapes[0].Kind
		got := EvaluateG5(&evidence)
		if got.Verdict != Fail || !strings.Contains(got.Detail, "duplicate shape kind") {
			t.Fatalf("got %+v", got)
		}
	})

	t.Run("bad sha256", func(t *testing.T) {
		evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
		evidence.WeightsSHA256 = "not-a-sha256"
		got := EvaluateG5(&evidence)
		if got.Verdict != Fail || !strings.Contains(got.Detail, "weights_sha256") {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestLoadCrossShapeEvidenceMissingAndEmpty(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	got, err := LoadCrossShapeEvidence(missing)
	if err != nil || got != nil {
		t.Fatalf("missing dir: evidence=%+v err=%v", got, err)
	}

	got, err = LoadCrossShapeEvidence(t.TempDir())
	if err != nil || got != nil {
		t.Fatalf("empty dir: evidence=%+v err=%v", got, err)
	}
}

func TestLoadCrossShapeEvidenceReturnsLatestMeasuredRun(t *testing.T) {
	dir := t.TempDir()
	older := validCrossShapeEvidence("2026-07-29T10:00:00-02:00")
	older.ActorKind = "human"
	newer := validCrossShapeEvidence("2026-07-29T12:00:01Z")
	newer.ActorKind = "agent"
	writeCrossShapeEvidence(t, filepath.Join(dir, "z-older.json"), older)
	writeCrossShapeEvidence(t, filepath.Join(dir, "a-newer.json"), newer)
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadCrossShapeEvidence(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got == nil || got.MeasuredAt != newer.MeasuredAt || got.ActorKind != "agent" {
		t.Fatalf("got %+v, want newer run", got)
	}
}

func TestCrossShapeFixtureSetHashIsPortableAndContentBound(t *testing.T) {
	writeSet := func(dir string) {
		t.Helper()
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "b.json"), []byte(`{"b":2}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "a.json"), []byte(`{"a":1}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	firstDir := filepath.Join(t.TempDir(), "one")
	secondDir := filepath.Join(t.TempDir(), "elsewhere")
	writeSet(firstDir)
	writeSet(secondDir)

	first, err := crossShapeFixtureSetSHA256(firstDir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := crossShapeFixtureSetSHA256(secondDir)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("same fixture set in different roots hashes differently: %s != %s", first, second)
	}
	if err := os.WriteFile(filepath.Join(secondDir, "a.json"), []byte(`{"a":3}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := crossShapeFixtureSetSHA256(secondDir)
	if err != nil {
		t.Fatal(err)
	}
	if changed == first {
		t.Fatal("fixture content change did not change set hash")
	}
}

func TestVerifyCrossShapeReportsChecksRetainedArtifacts(t *testing.T) {
	dir := t.TempDir()
	evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
	evidence.sourcePath = filepath.Join(dir, "run.json")
	current := writeValidCrossShapeReports(t, &evidence)
	if err := verifyCrossShapeReports(&evidence, current); err != nil {
		t.Fatalf("valid reports: %v", err)
	}

	reportDir := filepath.Join(dir, "reports", "run")
	tampered := filepath.Join(reportDir, CrossShapeKindPythonLibrary+"-gate.json")
	data, err := os.ReadFile(tampered)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tampered, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyCrossShapeReports(&evidence, current); err == nil ||
		!strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("tampered report error = %v", err)
	}
}

func TestVerifyCrossShapeReportsRejectsSemanticMismatchWithMatchingHash(t *testing.T) {
	dir := t.TempDir()
	evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
	evidence.sourcePath = filepath.Join(dir, "run.json")
	current := writeValidCrossShapeReports(t, &evidence)
	evidence.Shapes[0].Economy.Verdict = Fail

	err := verifyCrossShapeReports(&evidence, current)
	if err == nil || !strings.Contains(err.Error(), "verdict = PASS, evidence = FAIL") {
		t.Fatalf("semantic mismatch error = %v", err)
	}
}

func TestVerifyCrossShapeReportsRejectsUnboundEngineOrFixtureBundles(t *testing.T) {
	t.Run("engine source tree", func(t *testing.T) {
		dir := t.TempDir()
		evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
		evidence.sourcePath = filepath.Join(dir, "run.json")
		current := writeValidCrossShapeReports(t, &evidence)
		shape := &evidence.Shapes[0]
		reportDir := filepath.Join(dir, "reports", "run")

		economyPath := filepath.Join(reportDir, shape.Kind+"-economy.json")
		economyData, err := os.ReadFile(economyPath)
		if err != nil {
			t.Fatal(err)
		}
		var economy retainedEconomyReport
		if err := json.Unmarshal(economyData, &economy); err != nil {
			t.Fatal(err)
		}
		economy.G5Metadata.EngineSourceTreeSHA256 = strings.Repeat("a", 64)
		economyData, err = json.Marshal(economy)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(economyPath, economyData, 0o644); err != nil {
			t.Fatal(err)
		}
		shape.Economy.ReportSHA256 = mustCrossShapeFileHash(t, economyPath)

		gatePath := filepath.Join(reportDir, shape.Kind+"-gate.json")
		gateData, err := os.ReadFile(gatePath)
		if err != nil {
			t.Fatal(err)
		}
		var gateReport Report
		if err := json.Unmarshal(gateData, &gateReport); err != nil {
			t.Fatal(err)
		}
		gateReport.G5Metadata.EngineSourceTreeSHA256 = strings.Repeat("a", 64)
		gateData, err = json.Marshal(gateReport)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gatePath, gateData, 0o644); err != nil {
			t.Fatal(err)
		}
		shape.G2.ReportSHA256 = mustCrossShapeFileHash(t, gatePath)
		shape.G3.ReportSHA256 = shape.G2.ReportSHA256

		err = verifyCrossShapeReports(&evidence, current)
		if err == nil || !strings.Contains(err.Error(), "engine source-tree hash") {
			t.Fatalf("unbound engine source error = %v", err)
		}
	})

	t.Run("external target source tree", func(t *testing.T) {
		dir := t.TempDir()
		evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
		evidence.sourcePath = filepath.Join(dir, "run.json")
		current := writeValidCrossShapeReports(t, &evidence)
		shape := &evidence.Shapes[1]
		reportDir := filepath.Join(dir, "reports", "run")

		economyPath := filepath.Join(reportDir, shape.Kind+"-economy.json")
		economyData, err := os.ReadFile(economyPath)
		if err != nil {
			t.Fatal(err)
		}
		var economy retainedEconomyReport
		if err := json.Unmarshal(economyData, &economy); err != nil {
			t.Fatal(err)
		}
		economy.G5Metadata.TargetSourceTreeSHA256 = ""
		economyData, err = json.Marshal(economy)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(economyPath, economyData, 0o644); err != nil {
			t.Fatal(err)
		}
		shape.Economy.ReportSHA256 = mustCrossShapeFileHash(t, economyPath)

		gatePath := filepath.Join(reportDir, shape.Kind+"-gate.json")
		gateData, err := os.ReadFile(gatePath)
		if err != nil {
			t.Fatal(err)
		}
		var gateReport Report
		if err := json.Unmarshal(gateData, &gateReport); err != nil {
			t.Fatal(err)
		}
		gateReport.G5Metadata.TargetSourceTreeSHA256 = ""
		gateData, err = json.Marshal(gateReport)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gatePath, gateData, 0o644); err != nil {
			t.Fatal(err)
		}
		shape.G2.ReportSHA256 = mustCrossShapeFileHash(t, gatePath)
		shape.G3.ReportSHA256 = shape.G2.ReportSHA256

		err = verifyCrossShapeReports(&evidence, current)
		if err == nil || !strings.Contains(err.Error(), "target source-tree hash") {
			t.Fatalf("missing external target source error = %v", err)
		}
	})

	t.Run("fixture bundle coverage", func(t *testing.T) {
		dir := t.TempDir()
		evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
		evidence.sourcePath = filepath.Join(dir, "run.json")
		current := writeValidCrossShapeReports(t, &evidence)
		shape := &evidence.Shapes[0]
		gatePath := filepath.Join(dir, "reports", "run", shape.Kind+"-gate.json")
		data, err := os.ReadFile(gatePath)
		if err != nil {
			t.Fatal(err)
		}
		var report Report
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		report.G5GateInvocation.FixtureBundles =
			report.G5GateInvocation.FixtureBundles[:len(report.G5GateInvocation.FixtureBundles)-1]
		data, err = json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(gatePath, data, 0o644); err != nil {
			t.Fatal(err)
		}
		shape.G2.ReportSHA256 = mustCrossShapeFileHash(t, gatePath)
		shape.G3.ReportSHA256 = shape.G2.ReportSHA256

		err = verifyCrossShapeReports(&evidence, current)
		if err == nil || !strings.Contains(err.Error(), "fixture bundle count") {
			t.Fatalf("incomplete fixture-bundle error = %v", err)
		}
	})
}

func TestVerifyCrossShapeReportsRejectsRowsThatDoNotMatchFixtures(t *testing.T) {
	t.Run("economy task", func(t *testing.T) {
		dir := t.TempDir()
		evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
		evidence.sourcePath = filepath.Join(dir, "run.json")
		current := writeValidCrossShapeReports(t, &evidence)
		shape := &evidence.Shapes[0]
		path := filepath.Join(dir, "reports", "run", shape.Kind+"-economy.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var report retainedEconomyReport
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		report.Tasks[0] = abeval.TaskResult{}
		data, err = json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		shape.Economy.ReportSHA256 = mustCrossShapeFileHash(t, path)

		err = verifyCrossShapeReports(&evidence, current)
		if err == nil || !strings.Contains(err.Error(), "question does not match") {
			t.Fatalf("mismatched economy row error = %v", err)
		}
	})

	t.Run("G3 detail", func(t *testing.T) {
		dir := t.TempDir()
		evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
		evidence.sourcePath = filepath.Join(dir, "run.json")
		current := writeValidCrossShapeReports(t, &evidence)
		shape := &evidence.Shapes[0]
		path := filepath.Join(dir, "reports", "run", shape.Kind+"-gate.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var report Report
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		report.G3Details[0] = FactResult{}
		data, err = json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		shape.G2.ReportSHA256 = mustCrossShapeFileHash(t, path)
		shape.G3.ReportSHA256 = shape.G2.ReportSHA256

		err = verifyCrossShapeReports(&evidence, current)
		if err == nil || !strings.Contains(err.Error(), "fixture does not match") {
			t.Fatalf("mismatched G3 row error = %v", err)
		}
	})

	t.Run("runtime metadata", func(t *testing.T) {
		dir := t.TempDir()
		evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
		evidence.sourcePath = filepath.Join(dir, "run.json")
		current := writeValidCrossShapeReports(t, &evidence)
		shape := &evidence.Shapes[0]
		path := filepath.Join(dir, "reports", "run", shape.Kind+"-gate.json")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var report Report
		if err := json.Unmarshal(data, &report); err != nil {
			t.Fatal(err)
		}
		report.G5Metadata.BinarySHA256 = strings.Repeat("a", 64)
		data, err = json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		shape.G2.ReportSHA256 = mustCrossShapeFileHash(t, path)
		shape.G3.ReportSHA256 = shape.G2.ReportSHA256

		err = verifyCrossShapeReports(&evidence, current)
		if err == nil || !strings.Contains(err.Error(), "runtime metadata differ") {
			t.Fatalf("mismatched runtime metadata error = %v", err)
		}
	})
}

func TestCrossShapeSourceTreeHashTracksDirtyAndUntrackedInputs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}
	repo := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test/hash\n\ngo 1.25\n")
	write("main.go", "package main\n")
	runCrossShapeGit(t, repo, "init")
	runCrossShapeGit(t, repo, "add", "go.mod", "main.go")

	base, err := crossShapeSourceTreeSHA256(repo)
	if err != nil {
		t.Fatal(err)
	}
	write("main.go", "package main\n\nfunc main() {}\n")
	dirty, err := crossShapeSourceTreeSHA256(repo)
	if err != nil {
		t.Fatal(err)
	}
	if dirty == base {
		t.Fatal("tracked content change did not change source hash")
	}
	write("extra.go", "package main\n")
	untracked, err := crossShapeSourceTreeSHA256(repo)
	if err != nil {
		t.Fatal(err)
	}
	if untracked == dirty {
		t.Fatal("untracked Go input did not change source hash")
	}

	if err := os.MkdirAll(filepath.Join(repo, ".neurofs"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(".neurofs", "ranking_weights.json"), `{"filename":1}`)
	configured, err := crossShapeSourceTreeSHA256(repo)
	if err != nil {
		t.Fatal(err)
	}
	if configured == untracked {
		t.Fatal("local ranking configuration did not change source hash")
	}
}

func TestCrossShapeSourceTreeHashIsStableAcrossCommittedDeletion(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.test/hash\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deletedPath := filepath.Join(repo, "deleted.go")
	if err := os.WriteFile(deletedPath, []byte("package hash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runCrossShapeGit(t, repo, "init")
	runCrossShapeGit(t, repo, "config", "user.name", "Cross Shape Test")
	runCrossShapeGit(t, repo, "config", "user.email", "cross-shape@example.test")
	runCrossShapeGit(t, repo, "add", "go.mod", "deleted.go")
	runCrossShapeGit(t, repo, "commit", "-m", "initial")
	if err := os.Remove(deletedPath); err != nil {
		t.Fatal(err)
	}
	dirty, err := crossShapeSourceTreeSHA256(repo)
	if err != nil {
		t.Fatal(err)
	}
	runCrossShapeGit(t, repo, "add", "-u")
	runCrossShapeGit(t, repo, "commit", "-m", "delete source")
	committed, err := crossShapeSourceTreeSHA256(repo)
	if err != nil {
		t.Fatal(err)
	}
	if dirty != committed {
		t.Fatalf("effective tree hash changed after committing deletion: %s != %s", dirty, committed)
	}
}

func TestCrossShapeSourceTreeHashIncludesEmbeddedAssets(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go executable not available")
	}
	repo := t.TempDir()
	for _, dir := range []string{
		filepath.Join(repo, "cmd", "neurofs"),
		filepath.Join(repo, "internal", "assets", "static"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		"go.mod": "module example.test/hash\n\ngo 1.25\n",
		filepath.Join("cmd", "neurofs", "main.go"): `package main
import _ "example.test/hash/internal/assets"
func main() {}
`,
		filepath.Join("internal", "assets", "assets.go"): `package assets
import "embed"
//go:embed static
var Files embed.FS
`,
		filepath.Join("internal", "assets", "static", "app.js"): "console.log('one')\n",
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runCrossShapeGit(t, repo, "init")
	runCrossShapeGit(t, repo, "add", ".")

	before, err := crossShapeSourceTreeSHA256(repo)
	if err != nil {
		t.Fatal(err)
	}
	asset := filepath.Join(repo, "internal", "assets", "static", "app.js")
	if err := os.WriteFile(asset, []byte("console.log('two')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	after, err := crossShapeSourceTreeSHA256(repo)
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("embedded asset change did not change source hash")
	}
}

func TestCrossShapeTargetSourceTreeHashMakesIndexableDocChangesStale(t *testing.T) {
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	repo := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(repo, "answer.go"),
		[]byte("package answer\n\nconst Fact = true\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	docPath := filepath.Join(repo, "guide.md")
	if err := os.WriteFile(docPath, []byte("# Guide\n\nfirst generation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scan := func() {
		t.Helper()
		cfg, err := config.New(repo)
		if err != nil {
			t.Fatal(err)
		}
		db, err := storage.Open(cfg.DBPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	scan()
	before, err := crossShapeTargetSourceTreeSHA256(repo)
	if err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	evidence := validCrossShapeEvidence("2026-07-29T12:00:00Z")
	evidence.sourcePath = filepath.Join(dir, "run.json")
	current := writeValidCrossShapeReports(t, &evidence)
	shape := &evidence.Shapes[0]
	reportDir := filepath.Join(dir, "reports", "run")
	economyPath := filepath.Join(reportDir, shape.Kind+"-economy.json")
	economyData, err := os.ReadFile(economyPath)
	if err != nil {
		t.Fatal(err)
	}
	var economy retainedEconomyReport
	if err := json.Unmarshal(economyData, &economy); err != nil {
		t.Fatal(err)
	}
	economy.G5Metadata.TargetSourceTreeSHA256 = before
	economyData, err = json.Marshal(economy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(economyPath, economyData, 0o644); err != nil {
		t.Fatal(err)
	}
	shape.Economy.ReportSHA256 = mustCrossShapeFileHash(t, economyPath)

	gatePath := filepath.Join(reportDir, shape.Kind+"-gate.json")
	gateData, err := os.ReadFile(gatePath)
	if err != nil {
		t.Fatal(err)
	}
	var gateReport Report
	if err := json.Unmarshal(gateData, &gateReport); err != nil {
		t.Fatal(err)
	}
	gateReport.G5Metadata.TargetSourceTreeSHA256 = before
	gateData, err = json.Marshal(gateReport)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gatePath, gateData, 0o644); err != nil {
		t.Fatal(err)
	}
	shape.G2.ReportSHA256 = mustCrossShapeFileHash(t, gatePath)
	shape.G3.ReportSHA256 = shape.G2.ReportSHA256
	current.TargetSourceTrees[CrossShapeKindGoService] = before
	if err := verifyCrossShapeReports(&evidence, current); err != nil {
		t.Fatalf("reports should match initial target tree: %v", err)
	}

	if err := os.WriteFile(docPath, []byte("# Guide\n\nsecond generation\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := crossShapeTargetSourceTreeSHA256(repo); err == nil ||
		!strings.Contains(err.Error(), "target index is stale") {
		t.Fatalf("changed indexable doc stale error = %v", err)
	}
	scan()
	after, err := crossShapeTargetSourceTreeSHA256(repo)
	if err != nil {
		t.Fatal(err)
	}
	if after == before {
		t.Fatal("indexable Markdown change did not change target source-tree hash")
	}
	current.TargetSourceTrees[CrossShapeKindGoService] = after
	if err := verifyCrossShapeReports(&evidence, current); err == nil ||
		!strings.Contains(err.Error(), "current Go indexed tree") {
		t.Fatalf("changed target tree report error = %v", err)
	}
}

func TestLoadCrossShapeEvidenceRejectsSymlinkJSON(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.json")
	bad := validCrossShapeEvidence("not-rfc3339")
	writeCrossShapeEvidence(t, outside, bad)
	if err := os.Symlink(outside, filepath.Join(dir, "linked.json")); err != nil {
		t.Fatal(err)
	}

	got, err := LoadCrossShapeEvidence(dir)
	if err == nil || got != nil || !strings.Contains(err.Error(), "must not be a symlink") {
		t.Fatalf("symlink should be rejected: evidence=%+v err=%v", got, err)
	}
}

func TestCrossShapeEvidenceRequiresCanonicalRunConfig(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CrossShapeEvidence)
		want   string
	}{
		{
			name: "embedding model",
			mutate: func(e *CrossShapeEvidence) {
				e.RunConfig.EmbeddingModel = "other-model"
			},
			want: "embedding_model",
		},
		{
			name: "search limit",
			mutate: func(e *CrossShapeEvidence) {
				e.RunConfig.EconomySearchLimit = crossShapeEconomySearchLimit + 1
			},
			want: "economy_search_limit",
		},
		{
			name: "fixture budget",
			mutate: func(e *CrossShapeEvidence) {
				e.RunConfig.FixtureBudget = crossShapeFixtureBudget + 1
			},
			want: "fixture_budget",
		},
		{
			name: "chunks disabled",
			mutate: func(e *CrossShapeEvidence) {
				e.RunConfig.NoChunks = true
			},
			want: "no_chunks",
		},
		{
			name: "mock semantic override",
			mutate: func(e *CrossShapeEvidence) {
				e.RunConfig.MockSemantic = true
			},
			want: "mock_semantic",
		},
		{
			name: "uppercase hash",
			mutate: func(e *CrossShapeEvidence) {
				e.WeightsSHA256 = strings.ToUpper(e.WeightsSHA256)
			},
			want: "lowercase",
		},
		{
			name: "future timestamp",
			mutate: func(e *CrossShapeEvidence) {
				e.MeasuredAt = time.Now().Add(crossShapeMaxFutureSkew + time.Hour).UTC().Format(time.RFC3339)
			},
			want: "future",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evidence := validCrossShapeEvidence(time.Now().UTC().Format(time.RFC3339))
			test.mutate(&evidence)
			got := EvaluateG5(&evidence)
			if got.Verdict != Fail || !strings.Contains(got.Detail, test.want) {
				t.Fatalf("got %+v, want validation error containing %q", got, test.want)
			}
		})
	}
}

func TestCrossShapePinnedCommitRequiresOneUniquePin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "README.md")
	if err := os.WriteFile(path, []byte("pin `0123456789abcdef0123456789abcdef01234567`\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := crossShapePinnedCommit(path)
	if err != nil || got != "0123456789abcdef0123456789abcdef01234567" {
		t.Fatalf("pin = %q, err = %v", got, err)
	}
	if err := os.WriteFile(
		path,
		[]byte("`0123456789abcdef0123456789abcdef01234567` and `aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa`\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := crossShapePinnedCommit(path); err == nil {
		t.Fatal("multiple distinct pins should fail")
	}
}

func TestBuildCrossShapeReportMetadataCapturesRuntimeAndIgnoresNeuroFSState(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git executable not available")
	}
	t.Setenv("NEUROFS_EMBEDDING_PROVIDER", "mock")
	t.Setenv("NEUROFS_MOCK_SEMANTIC", "")
	repo := t.TempDir()
	fixturesDir := filepath.Join(repo, "fixtures")
	if err := os.MkdirAll(filepath.Join(repo, ".neurofs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(fixturesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.test/meta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package meta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte("ignored.go\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixturesDir, "one.json"), []byte(`{"fixture":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".neurofs", "weights.json"), []byte(`{"weight":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runCrossShapeGit(t, repo, "init")
	runCrossShapeGit(t, repo, "config", "user.name", "Cross Shape Test")
	runCrossShapeGit(t, repo, "config", "user.email", "cross-shape@example.test")
	runCrossShapeGit(t, repo, "remote", "add", "origin", "https://example.test/owner/repo.git")
	runCrossShapeGit(t, repo, "add", "go.mod", "main.go", ".gitignore", "fixtures")
	runCrossShapeGit(t, repo, "commit", "-m", "initial")
	scan := func() {
		t.Helper()
		cfg, err := config.New(repo)
		if err != nil {
			t.Fatal(err)
		}
		db, err := storage.Open(cfg.DBPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := indexer.Run(cfg, db, indexer.Options{}); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
	}
	scan()

	sourceHash, err := crossShapeSourceTreeSHA256(repo)
	if err != nil {
		t.Fatal(err)
	}
	oldBuildSourceHash := BuildSourceTreeSHA256
	BuildSourceTreeSHA256 = sourceHash
	t.Cleanup(func() { BuildSourceTreeSHA256 = oldBuildSourceHash })

	metadata, err := BuildCrossShapeReportMetadata(repo, fixturesDir, repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.EmbeddingProvider != "mock" ||
		metadata.EmbeddingModel != crossShapeEmbeddingModel ||
		metadata.MockSemantic ||
		metadata.RepoDirty {
		t.Fatalf("unexpected runtime metadata: %+v", metadata)
	}
	if len(metadata.BinarySHA256) != 64 ||
		len(metadata.EngineSourceTreeSHA256) != 64 ||
		len(metadata.TargetSourceTreeSHA256) != 64 ||
		len(metadata.WeightsSHA256) != 64 ||
		len(metadata.FixtureSetSHA256) != 64 ||
		len(metadata.RepoCommitSHA) != 40 {
		t.Fatalf("missing identity hashes: %+v", metadata)
	}
	if metadata.RepoRemoteURL != "https://example.test/owner/repo.git" {
		t.Fatalf("origin = %q", metadata.RepoRemoteURL)
	}
	BuildSourceTreeSHA256 = ""
	if _, err := BuildCrossShapeReportMetadata(repo, fixturesDir, repo, false); err == nil ||
		!strings.Contains(err.Error(), "no engine source-tree provenance") {
		t.Fatalf("unstamped executable error = %v", err)
	}
	BuildSourceTreeSHA256 = strings.Repeat("a", 64)
	if _, err := BuildCrossShapeReportMetadata(repo, fixturesDir, repo, false); err == nil ||
		!strings.Contains(err.Error(), "was built from engine source tree") {
		t.Fatalf("mismatched source stamp error = %v", err)
	}
	BuildSourceTreeSHA256 = sourceHash

	// Git's ordinary porcelain output omits ignored files. NeuroFS does not
	// use .gitignore as an indexing boundary, so such a file must still make
	// an external corpus dirty instead of silently changing the measured tree.
	if err := os.WriteFile(
		filepath.Join(repo, "ignored.go"),
		[]byte("package meta\n\nconst InjectedFact = true\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	scan()
	metadata, err = BuildCrossShapeReportMetadata(repo, fixturesDir, repo, false)
	if err != nil {
		t.Fatal(err)
	}
	if !metadata.RepoDirty {
		t.Fatal("ignored source file must mark the measured checkout dirty")
	}
}

func TestLoadCrossShapeEvidenceRejectsMalformedCandidate(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(dir, "bad.json"),
		[]byte(`{"schema_version":1,"unknown":true}`),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	got, err := LoadCrossShapeEvidence(dir)
	if err == nil || got != nil {
		t.Fatalf("evidence=%+v err=%v, want parse error", got, err)
	}
	if !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("error = %v, want unknown-field detail", err)
	}
}

func TestLoadCrossShapeEvidenceBoundsFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oversized.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxCrossShapeEvidenceBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	got, err := LoadCrossShapeEvidence(dir)
	if err == nil || got != nil {
		t.Fatalf("evidence=%+v err=%v, want size error", got, err)
	}
	if !errors.Is(err, fsutil.ErrFileTooLarge) {
		t.Fatalf("error = %v, want ErrFileTooLarge", err)
	}
}

func validCrossShapeEvidence(measuredAt string) CrossShapeEvidence {
	makeShape := func(name, kind string) CrossShapeShapeEvidence {
		return CrossShapeShapeEvidence{
			Name:             name,
			Kind:             kind,
			RepoURL:          "https://example.test/" + name,
			CommitSHA:        "0123456789abcdef0123456789abcdef01234567",
			FixtureSetSHA256: crossShapeTestHash,
			Economy: CrossShapeEconomyEvidence{
				Verdict:            Pass,
				MeanTokenReduction: 0.4,
				OverallRecall:      0.9,
				MissRate:           0.1,
				ReportSHA256:       crossShapeTestHash,
			},
			G2: CrossShapeG2Evidence{
				Verdict:      Pass,
				Bundles:      5,
				Overshoots:   0,
				ReportSHA256: crossShapeTestHash,
			},
			G3: CrossShapeG3Evidence{
				Verdict:      Pass,
				MeanRecall:   0.8,
				Fixtures:     5,
				Perfect:      4,
				ReportSHA256: crossShapeTestHash,
			},
			Bench: &CrossShapeBenchEvidence{
				Questions:    3,
				Top3Percent:  66.7,
				ReportSHA256: crossShapeTestHash,
			},
		}
	}
	return CrossShapeEvidence{
		SchemaVersion:    CrossShapeSchemaVersion,
		MeasuredAt:       measuredAt,
		ActorKind:        "agent",
		SignalKind:       CrossShapeSignalKind,
		BinarySHA256:     crossShapeTestHash,
		WeightsSHA256:    crossShapeTestHash,
		SourceTreeSHA256: crossShapeTestHash,
		RunConfig: &CrossShapeRunConfig{
			EmbeddingProvider:  "mock",
			EmbeddingModel:     crossShapeEmbeddingModel,
			EconomySearchLimit: 8,
			FixtureBudget:      8000,
		},
		Shapes: []CrossShapeShapeEvidence{
			makeShape("neurofs", CrossShapeKindGoService),
			makeShape("click", CrossShapeKindPythonLibrary),
			makeShape("vue", CrossShapeKindTypeScriptFrontend),
		},
	}
}

func runCrossShapeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func writeCrossShapeEvidence(t *testing.T, path string, evidence CrossShapeEvidence) {
	t.Helper()
	data, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func writeValidCrossShapeReports(
	t *testing.T,
	evidence *CrossShapeEvidence,
) CrossShapeCurrentIdentity {
	t.Helper()
	reportDir := filepath.Join(
		filepath.Dir(evidence.sourcePath),
		"reports",
		strings.TrimSuffix(filepath.Base(evidence.sourcePath), filepath.Ext(evidence.sourcePath)),
	)
	if err := os.MkdirAll(reportDir, 0o755); err != nil {
		t.Fatal(err)
	}
	current := CrossShapeCurrentIdentity{
		Fixtures: make(map[string][]Fixture),
		TargetSourceTrees: map[string]string{
			CrossShapeKindGoService: crossShapeTestHash,
		},
	}
	for i := range evidence.Shapes {
		shape := &evidence.Shapes[i]
		fixtures := crossShapeReportTestFixtures(shape.Kind, crossShapeMinFixtures)
		current.Fixtures[shape.Kind] = fixtures

		tasks := make([]abeval.TaskResult, 0, len(fixtures))
		details := make([]FactResult, 0, len(fixtures))
		for _, fixture := range fixtures {
			facts := nonBlankFixtureFacts(fixture.ExpectsFacts)
			tasks = append(tasks, abeval.TaskResult{
				Question: fixture.Question,
				Source:   filepath.Base(fixture.SourcePath),
				Neurofs: abeval.Arm{
					Tokens:   50,
					Recall:   1,
					Files:    []string{"answer.go"},
					FactsHit: append([]string(nil), facts...),
				},
				NativeIso: abeval.Arm{
					Tokens:   100,
					Recall:   1,
					Files:    []string{"answer.go"},
					FactsHit: append([]string(nil), facts...),
				},
				TokenReduction: 0.5,
				HasFacts:       true,
				Scored:         true,
			})
			details = append(details, FactResult{
				Fixture: fixture,
				Recall:  1,
				Hits:    append([]string(nil), facts...),
			})
		}

		summary := abeval.Summarise(tasks, abeval.Options{
			SearchLimit: evidence.RunConfig.EconomySearchLimit,
			Threshold:   crossShapeMinTokenReduction,
		})
		shape.Economy.Verdict = Verdict(summary.Verdict)
		shape.Economy.MeanTokenReduction = summary.MeanTokenReduction
		shape.Economy.OverallRecall = summary.OverallRecallNeurofs
		shape.Economy.MissRate = summary.MissRate
		metadata := &CrossShapeReportMetadata{
			BinarySHA256:           evidence.BinarySHA256,
			EngineSourceTreeSHA256: evidence.SourceTreeSHA256,
			TargetSourceTreeSHA256: crossShapeTestHash,
			WeightsSHA256:          evidence.WeightsSHA256,
			FixtureSetSHA256:       shape.FixtureSetSHA256,
			EmbeddingProvider:      evidence.RunConfig.EmbeddingProvider,
			EmbeddingModel:         evidence.RunConfig.EmbeddingModel,
			MockSemantic:           evidence.RunConfig.MockSemantic,
			RepoCommitSHA:          shape.CommitSHA,
			RepoRemoteURL:          shape.RepoURL,
		}
		economy := retainedEconomyReport{
			Repo:        filepath.Join("/tmp", shape.Name),
			SearchLimit: evidence.RunConfig.EconomySearchLimit,
			Summary:     summary,
			Tasks:       tasks,
			G5Metadata:  metadata,
		}
		economyData, err := json.Marshal(economy)
		if err != nil {
			t.Fatal(err)
		}
		economyPath := filepath.Join(reportDir, shape.Kind+"-economy.json")
		if err := os.WriteFile(economyPath, economyData, 0o644); err != nil {
			t.Fatal(err)
		}
		shape.Economy.ReportSHA256 = mustCrossShapeFileHash(t, economyPath)

		g3 := EvaluateG3(details, DefaultG3Thresholds())
		shape.G3.Verdict = g3.Verdict
		shape.G3.Fixtures = len(details)
		shape.G3.Perfect = int(g3.Numbers["perfect"])
		shape.G3.MeanRecall = g3.Numbers["mean_recall"]
		fixtureBundles := make([]CrossShapeFixtureBundleAttestation, 0, len(fixtures))
		for j, fixture := range fixtures {
			questionHash := sha256.Sum256([]byte(fixture.Question))
			fixtureBundles = append(fixtureBundles, CrossShapeFixtureBundleAttestation{
				FixtureSource:  filepath.Base(fixture.SourcePath),
				QuestionSHA256: hex.EncodeToString(questionHash[:]),
				BundleHash:     fmt.Sprintf("%064x", j+1),
				TokensUsed:     50,
				TokensBudget:   evidence.RunConfig.FixtureBudget,
			})
		}
		shape.G2.Bundles = len(fixtureBundles)
		shape.G2.Overshoots = 0
		gateReport := Report{
			Criteria: []Criterion{
				{
					ID:      "G2",
					Verdict: shape.G2.Verdict,
					Numbers: map[string]float64{
						"bundles":    float64(shape.G2.Bundles),
						"overshoots": float64(shape.G2.Overshoots),
					},
				},
				g3,
			},
			G3Details:  details,
			G5Metadata: metadata,
			G5GateInvocation: &CrossShapeGateInvocation{
				FixtureBudget:  evidence.RunConfig.FixtureBudget,
				NoChunks:       evidence.RunConfig.NoChunks,
				FixtureBundles: fixtureBundles,
			},
		}
		gateData, err := json.Marshal(gateReport)
		if err != nil {
			t.Fatal(err)
		}
		gatePath := filepath.Join(reportDir, shape.Kind+"-gate.json")
		if err := os.WriteFile(gatePath, gateData, 0o644); err != nil {
			t.Fatal(err)
		}
		shape.G2.ReportSHA256 = mustCrossShapeFileHash(t, gatePath)
		shape.G3.ReportSHA256 = shape.G2.ReportSHA256

		benchPath := filepath.Join(reportDir, shape.Kind+"-bench.txt")
		benchData := []byte("  questions : 3\n\n  summary:\n    top-3     : 66.7%\n")
		if err := os.WriteFile(benchPath, benchData, 0o644); err != nil {
			t.Fatal(err)
		}
		shape.Bench.ReportSHA256 = mustCrossShapeFileHash(t, benchPath)
	}
	return current
}

func crossShapeReportTestFixtures(kind string, count int) []Fixture {
	fixtures := make([]Fixture, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("%s-%02d.json", kind, i)
		fixtures = append(fixtures, Fixture{
			Question:     fmt.Sprintf("question %s %d", kind, i),
			ExpectsFacts: []string{fmt.Sprintf("fact_%d", i)},
			Source:       "test",
			SourcePath:   filepath.Join("/fixtures", name),
		})
	}
	return fixtures
}

func mustCrossShapeFileHash(t *testing.T, path string) string {
	t.Helper()
	hash, err := crossShapeFileSHA256(path)
	if err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return hash
}

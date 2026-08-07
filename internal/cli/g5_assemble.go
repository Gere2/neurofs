package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"

	"github.com/Gere2/neurofs/internal/gate"
	"github.com/spf13/cobra"
)

// g5ShapeOrder fixes the order shapes appear in the evidence so two runs over
// the same reports produce byte-identical documents.
var g5ShapeOrder = []struct {
	Kind string
	Name string
}{
	{"go_service", "NeuroFS"},
	{"python_library", "pallets/click"},
	{"typescript_frontend", "vuejs/core"},
}

// newG5AssembleCmd builds a schema-v2 cross-shape evidence document from the
// per-shape reports a measurement run produced. Hidden, like g5-source-hash:
// it exists so `make g5-remeasure` does not have to reimplement the schema in
// shell, which is how the evidence used to be written and how it drifted.
//
// It only transcribes: every number is read back out of the retained reports,
// and the reports' own --g5-attest metadata supplies the run identity. There
// is deliberately no way to pass a verdict in by hand.
func newG5AssembleCmd() *cobra.Command {
	var reportsDir, out string
	cmd := &cobra.Command{
		Use:    "g5-assemble",
		Hidden: true,
		Args:   cobra.NoArgs,
		Short:  "Assemble G5 cross-shape evidence from retained per-shape reports",
		RunE: func(cmd *cobra.Command, args []string) error {
			ev, err := assembleG5Evidence(reportsDir)
			if err != nil {
				return err
			}
			data, err := json.MarshalIndent(ev, "", " ")
			if err != nil {
				return fmt.Errorf("g5-assemble: encode evidence: %w", err)
			}
			data = append(data, '\n')
			if out == "" {
				_, err = cmd.OutOrStdout().Write(data)
				return err
			}
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return fmt.Errorf("g5-assemble: create output directory: %w", err)
			}
			return os.WriteFile(out, data, 0o644)
		},
	}
	cmd.Flags().StringVar(&reportsDir, "reports-dir", "",
		"Directory holding <shape>-{economy,gate,bench}.{json,txt} for one run")
	cmd.Flags().StringVar(&out, "out", "", "Write the evidence JSON here (default stdout)")
	_ = cmd.MarkFlagRequired("reports-dir")
	return cmd
}

// g5EconomyReport is the subset of an `economy --out` report the evidence
// quotes, plus the attestation metadata --g5-attest embeds.
type g5EconomyReport struct {
	SearchLimit int `json:"search_limit"`
	Summary     struct {
		Verdict              string  `json:"verdict"`
		MeanTokenReduction   float64 `json:"mean_token_reduction"`
		OverallRecallNeurofs float64 `json:"overall_recall_neurofs"`
		MissRate             float64 `json:"miss_rate"`
	} `json:"summary"`
	Metadata *gate.CrossShapeReportMetadata `json:"g5_metadata"`
}

// g5GateReport is the subset of a `gate --out` report the evidence quotes.
// The invocation block matters as much as the criteria: the verifier compares
// evidence.RunConfig against it, so the budget is read back from the run that
// actually happened rather than assumed.
type g5GateReport struct {
	Criteria []struct {
		ID      string             `json:"id"`
		Verdict string             `json:"verdict"`
		Numbers map[string]float64 `json:"numbers"`
	} `json:"criteria"`
	Metadata   *gate.CrossShapeReportMetadata `json:"g5_metadata"`
	Invocation *gate.CrossShapeGateInvocation `json:"g5_gate_invocation"`
}

var (
	g5BenchQuestions = regexp.MustCompile(`questions\s*:\s*(\d+)`)
	g5BenchTop3      = regexp.MustCompile(`top-3\s*:\s*([\d.]+)%`)
)

func assembleG5Evidence(reportsDir string) (*gate.CrossShapeEvidence, error) {
	if reportsDir == "" {
		return nil, fmt.Errorf("g5-assemble: --reports-dir is required")
	}
	ev := &gate.CrossShapeEvidence{
		SchemaVersion: 2,
		MeasuredAt:    time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		ActorKind:     "agent",
		SignalKind:    gate.CrossShapeSignalKind,
	}

	for _, shape := range g5ShapeOrder {
		economyPath := filepath.Join(reportsDir, shape.Kind+"-economy.json")
		gatePath := filepath.Join(reportsDir, shape.Kind+"-gate.json")

		var economy g5EconomyReport
		economyHash, err := readG5JSON(economyPath, &economy)
		if err != nil {
			return nil, err
		}
		var gateReport g5GateReport
		gateHash, err := readG5JSON(gatePath, &gateReport)
		if err != nil {
			return nil, err
		}
		if economy.Metadata == nil || gateReport.Metadata == nil {
			return nil, fmt.Errorf("g5-assemble: %s reports lack --g5-attest metadata", shape.Kind)
		}
		if gateReport.Invocation == nil {
			return nil, fmt.Errorf("g5-assemble: %s gate report lacks --g5-attest invocation", shape.Kind)
		}
		if *economy.Metadata != *gateReport.Metadata {
			return nil, fmt.Errorf("g5-assemble: %s economy and gate metadata disagree; "+
				"they must come from one measurement run", shape.Kind)
		}
		meta := economy.Metadata

		// The run identity is shared across shapes; take it from the first and
		// require the rest to agree, so a half-remeasured set cannot be
		// stitched into a document that looks whole.
		if ev.BinarySHA256 == "" {
			ev.BinarySHA256 = meta.BinarySHA256
			ev.WeightsSHA256 = meta.WeightsSHA256
			ev.SourceTreeSHA256 = meta.EngineSourceTreeSHA256
			ev.RunConfig = &gate.CrossShapeRunConfig{
				EmbeddingProvider:  meta.EmbeddingProvider,
				EmbeddingModel:     meta.EmbeddingModel,
				EconomySearchLimit: economy.SearchLimit,
				FixtureBudget:      gateReport.Invocation.FixtureBudget,
				NoChunks:           gateReport.Invocation.NoChunks,
				MockSemantic:       meta.MockSemantic,
			}
		} else if meta.BinarySHA256 != ev.BinarySHA256 ||
			meta.WeightsSHA256 != ev.WeightsSHA256 ||
			meta.EngineSourceTreeSHA256 != ev.SourceTreeSHA256 {
			return nil, fmt.Errorf("g5-assemble: %s was measured with a different "+
				"binary/weights/source tree than the earlier shapes", shape.Kind)
		}

		g2, err := g5Criterion(gateReport, "G2")
		if err != nil {
			return nil, fmt.Errorf("g5-assemble: %s: %w", shape.Kind, err)
		}
		g3, err := g5Criterion(gateReport, "G3")
		if err != nil {
			return nil, fmt.Errorf("g5-assemble: %s: %w", shape.Kind, err)
		}

		entry := gate.CrossShapeShapeEvidence{
			Name:             shape.Name,
			Kind:             shape.Kind,
			RepoURL:          meta.RepoRemoteURL,
			CommitSHA:        meta.RepoCommitSHA,
			FixtureSetSHA256: meta.FixtureSetSHA256,
			Economy: gate.CrossShapeEconomyEvidence{
				Verdict:            gate.Verdict(economy.Summary.Verdict),
				MeanTokenReduction: economy.Summary.MeanTokenReduction,
				OverallRecall:      economy.Summary.OverallRecallNeurofs,
				MissRate:           economy.Summary.MissRate,
				ReportSHA256:       economyHash,
			},
			G2: gate.CrossShapeG2Evidence{
				Verdict:      gate.Verdict(g2.Verdict),
				Bundles:      int(g2.Numbers["bundles"]),
				Overshoots:   int(g2.Numbers["overshoots"]),
				ReportSHA256: gateHash,
			},
			G3: gate.CrossShapeG3Evidence{
				Verdict:      gate.Verdict(g3.Verdict),
				MeanRecall:   g3.Numbers["mean_recall"],
				Fixtures:     int(g3.Numbers["fixtures"]),
				Perfect:      int(g3.Numbers["perfect"]),
				ReportSHA256: gateHash,
			},
		}

		bench, benchHash, err := readG5Bench(filepath.Join(reportsDir, shape.Kind+"-bench.txt"))
		if err != nil {
			return nil, err
		}
		if bench != nil {
			bench.ReportSHA256 = benchHash
			entry.Bench = bench
		}

		ev.Shapes = append(ev.Shapes, entry)
	}

	sort.SliceStable(ev.Shapes, func(i, j int) bool { return ev.Shapes[i].Kind < ev.Shapes[j].Kind })
	return ev, nil
}

type g5CriterionRow struct {
	Verdict string
	Numbers map[string]float64
}

func g5Criterion(report g5GateReport, id string) (g5CriterionRow, error) {
	for _, c := range report.Criteria {
		if c.ID == id {
			return g5CriterionRow{Verdict: c.Verdict, Numbers: c.Numbers}, nil
		}
	}
	return g5CriterionRow{}, fmt.Errorf("gate report has no %s row", id)
}

// readG5JSON decodes path into v and returns the file's SHA-256, which is the
// hash the evidence records and the gate later re-checks.
func readG5JSON(path string, v any) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("g5-assemble: read %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return "", fmt.Errorf("g5-assemble: parse %s: %w", filepath.Base(path), err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// readG5Bench parses the supplementary file-ranker report. A missing bench is
// not an error: Bench is optional in the schema.
func readG5Bench(path string) (*gate.CrossShapeBenchEvidence, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, "", nil
		}
		return nil, "", fmt.Errorf("g5-assemble: read %s: %w", filepath.Base(path), err)
	}
	text := string(data)
	qm := g5BenchQuestions.FindStringSubmatch(text)
	tm := g5BenchTop3.FindStringSubmatch(text)
	if qm == nil || tm == nil {
		return nil, "", fmt.Errorf("g5-assemble: %s has no question count or top-3 line", filepath.Base(path))
	}
	questions, err := strconv.Atoi(qm[1])
	if err != nil {
		return nil, "", fmt.Errorf("g5-assemble: %s question count: %w", filepath.Base(path), err)
	}
	top3, err := strconv.ParseFloat(tm[1], 64)
	if err != nil {
		return nil, "", fmt.Errorf("g5-assemble: %s top-3: %w", filepath.Base(path), err)
	}
	sum := sha256.Sum256(data)
	return &gate.CrossShapeBenchEvidence{Questions: questions, Top3Percent: top3},
		hex.EncodeToString(sum[:]), nil
}

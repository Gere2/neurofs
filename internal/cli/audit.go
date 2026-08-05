package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/Gere2/neurofs/internal/atomicfile"
	"github.com/Gere2/neurofs/internal/audit"
	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/embeddings"
	"github.com/Gere2/neurofs/internal/fsutil"
	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/packager"
	"github.com/Gere2/neurofs/internal/ranking"
	"github.com/Gere2/neurofs/internal/storage"
	"github.com/spf13/cobra"
)

const (
	maxAuditResponseBytes int64 = 16 << 20
	maxAuditBundleBytes   int64 = 64 << 20
	maxAuditFactsBytes    int64 = 4 << 20
)

// newAuditCmd is the parent command that groups every governance operation.
// Today only `replay` lives under it; when Anthropic integration lands it
// will host `run`, `diff`, etc. without reshuffling the CLI surface.
func newAuditCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Governance tools: measure whether responses stay within the bundle",
		Long: `Audit closes the loop between NeuroFS bundles and real model answers.
Subcommands operate without network access: you paste Claude's answer into
a file, then hand it to audit, which parses citations, checks drift, and
scores optional expected facts.`,
	}
	cmd.AddCommand(newAuditReplayCmd())
	cmd.AddCommand(newAuditDiffCmd())
	return cmd
}

// newAuditDiffCmd compares two persisted audit records. Typical use: run
// bench → replay → save a record, change the ranker, run again, diff the
// two. The numbers tell you if the change improved or regressed grounding.
func newAuditDiffCmd() *cobra.Command {
	var jsonOut string
	cmd := &cobra.Command{
		Use:   "diff <rec-a> <rec-b>",
		Short: "Compare two audit records and show the delta",
		Long: `Diff loads two AuditRecord JSON files and reports the before/after
change in grounded_ratio, drift_rate and (when applicable) fact_recall.
For each drift bucket (paths, apis, symbols) it shows the symbols added
in B and the ones removed from A.

The command is offline: no model calls, no index access. A differing
bundle hash is surfaced, not rejected — comparing across a reindex is
a legitimate workflow ("did the new index cost us grounding?").`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := audit.LoadRecord(args[0])
			if err != nil {
				return fmt.Errorf("audit diff: %w", err)
			}
			b, err := audit.LoadRecord(args[1])
			if err != nil {
				return fmt.Errorf("audit diff: %w", err)
			}

			d := audit.DiffRecords(a, b)
			if err := printDiffSummary(cmd.ErrOrStderr(), d, args[0], args[1]); err != nil {
				return fmt.Errorf("audit diff: write summary: %w", err)
			}

			if jsonOut != "" {
				if err := writeJSON(jsonOut, d); err != nil {
					return fmt.Errorf("audit diff: --json: %w", err)
				}
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "  json diff  : %s\n", jsonOut); err != nil {
					return fmt.Errorf("audit diff: write json path: %w", err)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&jsonOut, "json", "", "Also write the full Diff as JSON to this path")
	return cmd
}

// printDiffSummary renders the human-readable diff. We lead with identity
// (question/model/bundle) so the reader can judge whether A and B are even
// comparable before interpreting the numbers.
func printDiffSummary(dst io.Writer, d audit.Diff, aPath, bPath string) error {
	w := newReportWriter(dst)
	w.printf("\nNeuroFS — audit diff\n\n")
	w.printf("  A : %s\n", aPath)
	w.printf("  B : %s\n", bPath)

	if d.SameQuestion {
		w.printf("  question : %s\n", truncateLine(d.A.Question, 70))
	} else {
		w.printf("  question : DIFFERENT\n")
		w.printf("    A: %s\n", truncateLine(d.A.Question, 70))
		w.printf("    B: %s\n", truncateLine(d.B.Question, 70))
	}
	if d.SameModel {
		w.printf("  model    : %s\n", d.A.Model)
	} else {
		w.printf("  model    : %s → %s\n", d.A.Model, d.B.Model)
	}
	if d.SameBundle {
		w.printf("  bundle   : same (%s)\n", shortHash(d.A.BundleHash))
	} else {
		w.printf("  bundle   : DIFFERENT (%s → %s)\n",
			shortHash(d.A.BundleHash), shortHash(d.B.BundleHash))
	}

	w.printf("\n  grounded : %5.1f%% → %5.1f%%   (%+5.1f)\n",
		d.A.GroundedRatio*100, d.B.GroundedRatio*100, d.GroundedDelta*100)
	w.printf("  drift    : %5.1f%% → %5.1f%%   (%+5.1f)\n",
		d.A.Drift.Rate*100, d.B.Drift.Rate*100, d.DriftDelta*100)
	if d.RecallApplies {
		w.printf("  recall   : %5.1f%% → %5.1f%%   (%+5.1f)\n",
			d.A.AnswerRecall*100, d.B.AnswerRecall*100, d.RecallDelta*100)
	}

	printBucketDiff(w, "paths", d.Paths)
	printBucketDiff(w, "apis", d.APIs)
	printBucketDiff(w, "symbols", d.Symbols)
	w.printf("\n")
	return w.err
}

// printBucketDiff renders one bucket's added/removed lists, or stays silent
// when both are empty. "+" = appeared in B (potentially worse); "-" = gone
// from A (potentially better). We intentionally do not colour the output —
// terminals without ANSI support still read clearly.
func printBucketDiff(w *reportWriter, label string, sd audit.SetDiff) {
	if len(sd.Added) == 0 && len(sd.Removed) == 0 {
		return
	}
	w.printf("\n  %s:\n", label)
	for _, s := range sd.Added {
		w.printf("    +  %s\n", s)
	}
	for _, s := range sd.Removed {
		w.printf("    -  %s\n", s)
	}
}

// shortHash truncates a sha256 to 12 chars + ellipsis, matching the format
// used in `audit replay`. Empty input renders as "—" so the terminal does
// not show a dangling "bundle :" with nothing after it.
func shortHash(h string) string {
	if h == "" {
		return "—"
	}
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

// newAuditReplayCmd wires the manual-replay workflow. Two entry points are
// accepted so users can either re-pack from an existing index or load an
// already-packaged bundle JSON (useful when the index has moved on since
// the bundle was generated).
//
//	neurofs audit replay "<question>" --response answer.txt --repo .
//	neurofs audit replay --bundle bundle.json --response answer.txt
func newAuditReplayCmd() *cobra.Command {
	var (
		responsePath string
		bundlePath   string
		repoPath     string
		budget       int
		focus        string
		changedFlag  bool
		maxFiles     int
		maxFragments int
		modelID      string
		factsCSV     string
		factsFile    string
		save         bool
		recordsDir   string
		jsonOut      string
	)

	cmd := &cobra.Command{
		Use:   "replay [question]",
		Short: "Score a pasted model response against a NeuroFS bundle",
		Long: `Replay reads a response file, rebuilds (or loads) the bundle, and runs
the audit pipeline: citation parse + validate, drift detection, and
optional fact recall. Nothing is sent over the network — the pasted
response is treated as the model's output.

Two equivalent forms:

  # Recompute the bundle from the current index:
  neurofs audit replay "how does auth work" --response answer.txt

  # Audit against a previously saved bundle (see 'pack --save-bundle'):
  neurofs audit replay --bundle ctx.bundle.json --response answer.txt

Pass --save to persist a JSON record under audit/records/.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if responsePath == "" {
				return fmt.Errorf("audit replay: --response <file> is required")
			}
			if bundlePath == "" && len(args) == 0 {
				return fmt.Errorf("audit replay: pass a question as positional arg or use --bundle <file>")
			}
			if err := validateBudget(budget); err != nil {
				return fmt.Errorf("audit replay: %w", err)
			}

			response, _, err := fsutil.ReadRegularFileBounded(responsePath, maxAuditResponseBytes)
			if err != nil {
				return fmt.Errorf("audit replay: read response: %w", err)
			}

			var (
				bundle models.Bundle
				cfg    *config.Config
			)
			switch {
			case bundlePath != "":
				bundle, err = loadBundleJSON(bundlePath)
				if err != nil {
					return fmt.Errorf("audit replay: %w", err)
				}
				cfg, _ = config.New(repoPath) // optional — only used for records dir
			default:
				cfg, err = config.New(repoPath)
				if err != nil {
					return fmt.Errorf("audit replay: %w", err)
				}
				if err := cfg.Validate(); err != nil {
					return fmt.Errorf("audit replay: config: %w", err)
				}
				bundle, err = rebuildBundle(cfg, args[0], budget, focus, changedFlag, maxFiles, maxFragments)
				if err != nil {
					return fmt.Errorf("audit replay: %w", err)
				}
			}

			facts, err := parseFacts(factsCSV, factsFile)
			if err != nil {
				return fmt.Errorf("audit replay: %w", err)
			}

			rec, err := audit.Run(cmd.Context(),
				audit.StubModel{Label: modelID, Response: string(response)},
				bundle,
				audit.Options{ExpectsFacts: facts},
			)
			if err != nil {
				return fmt.Errorf("audit replay: %w", err)
			}

			if err := printReplaySummary(cmd.ErrOrStderr(), rec); err != nil {
				return fmt.Errorf("audit replay: write summary: %w", err)
			}

			if jsonOut != "" {
				if err := writeJSON(jsonOut, rec); err != nil {
					return fmt.Errorf("audit replay: --json: %w", err)
				}
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "  json record: %s\n", jsonOut); err != nil {
					return fmt.Errorf("audit replay: write json path: %w", err)
				}
			}

			if save {
				dir := recordsDir
				if dir == "" {
					root := "."
					if cfg != nil {
						root = cfg.RepoRoot
					}
					dir = filepath.Join(root, audit.DefaultRecordsDir)
				}
				path, err := audit.SaveRecordContext(cmd.Context(), dir, rec)
				if err != nil {
					return fmt.Errorf("audit replay: save: %w", err)
				}
				if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "  saved to   : %s\n", path); err != nil {
					return fmt.Errorf("audit replay: write record path: %w", err)
				}
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&responsePath, "response", "", "Path to the pasted model response (required)")
	cmd.Flags().StringVar(&bundlePath, "bundle", "", "Path to a bundle JSON file (from pack --save-bundle)")
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (required when rebuilding the bundle)")
	cmd.Flags().IntVar(&budget, "budget", config.DefaultBudget, "Token budget when rebuilding the bundle")
	cmd.Flags().StringVar(&focus, "focus", "", "Ranking focus prefix(es) used when rebuilding")
	cmd.Flags().BoolVar(&changedFlag, "changed", false, "Boost git-changed files when rebuilding")
	cmd.Flags().IntVar(&maxFiles, "max-files", 0, "Max files when rebuilding")
	cmd.Flags().IntVar(&maxFragments, "max-fragments", 0, "Max fragments when rebuilding")
	cmd.Flags().StringVar(&modelID, "model", "claude-manual", "Model id recorded in the AuditRecord (no network call is made)")
	cmd.Flags().StringVar(&factsCSV, "facts", "", "Comma-separated expected facts for recall scoring")
	cmd.Flags().StringVar(&factsFile, "facts-file", "", "Path to a text file with one expected fact per line")
	cmd.Flags().BoolVar(&save, "save", false, "Persist the record under audit/records/")
	cmd.Flags().StringVar(&recordsDir, "records-dir", "", "Override the persistence directory (default: <repo>/audit/records)")
	cmd.Flags().StringVar(&jsonOut, "json", "", "Also write the full AuditRecord JSON to this path")
	_ = cmd.MarkFlagRequired("response")

	return cmd
}

// loadBundleJSON reads a models.Bundle from a JSON file. This is the format
// produced by `pack --save-bundle`: a plain JSON dump of the Bundle struct.
func loadBundleJSON(path string) (models.Bundle, error) {
	var b models.Bundle
	data, _, err := fsutil.ReadRegularFileBounded(path, maxAuditBundleBytes)
	if err != nil {
		return b, fmt.Errorf("load bundle: %w", err)
	}
	if err := json.Unmarshal(data, &b); err != nil {
		return b, fmt.Errorf("parse bundle: %w", err)
	}
	if len(b.Fragments) == 0 {
		return b, fmt.Errorf("bundle at %s has no fragments", path)
	}
	return b, nil
}

// rebuildBundle runs the same pipeline as `pack` so the replay scores the
// exact artefact the user would regenerate today. The audit is then as good
// as the index and the flags — if the repo has moved on since the user
// asked Claude, recompute will not match the original; callers who want
// exact replay should use --bundle.
func rebuildBundle(cfg *config.Config, query string, budget int, focus string, changedFlag bool, maxFiles, maxFragments int) (bundle models.Bundle, retErr error) {
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return models.Bundle{}, fmt.Errorf("open index (did you run 'neurofs scan'?): %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			retErr = errors.Join(retErr, fmt.Errorf("close index: %w", err))
		}
	}()

	files, err := db.AllFiles()
	if err != nil {
		return models.Bundle{}, err
	}

	embClient := embeddings.NewClient(cfg.HybridMode)
	queryEmb, _ := embClient.GetEmbedding(context.Background(), query)
	fileEmbs, _ := db.AllEmbeddings()

	rels, _ := db.AllRelations()
	rankWeights, _, _ := ranking.LoadWeights(cfg.RepoRoot)
	rankOpts := ranking.Options{
		Project:        loadProjectInfo(db),
		Focus:          focus,
		QueryEmbedding: queryEmb,
		Embeddings:     fileEmbs,
		Relations:      rels,
		Weights:        &rankWeights,
	}
	if changedFlag {
		rankOpts.ChangedFiles = gitChangedFiles(cfg.RepoRoot)
	}
	ranked := ranking.RankWithOptions(files, query, rankOpts)

	return packager.Pack(ranked, query, packager.Options{
		Budget:           budget,
		MaxFiles:         maxFiles,
		MaxFragments:     maxFragments,
		PreferSignatures: true, // replay always treats bundles as Claude-shaped
		UpgradeWithSlack: true, // match taskflow.Run so replay reflects what task would emit today
		QueryTerms:       ranking.Tokenise(query),
	})
}

// parseFacts merges --facts and --facts-file. Both are optional; an empty
// result means the audit skips fact recall (ratio stays at 0, unreported).
func parseFacts(csv, file string) ([]string, error) {
	var out []string
	if strings.TrimSpace(csv) != "" {
		for _, f := range strings.Split(csv, ",") {
			if s := strings.TrimSpace(f); s != "" {
				out = append(out, s)
			}
		}
	}
	if file != "" {
		data, _, err := fsutil.ReadRegularFileBounded(file, maxAuditFactsBytes)
		if err != nil {
			return nil, fmt.Errorf("read --facts-file %s: %w", file, err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if s := strings.TrimSpace(line); s != "" {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// printReplaySummary renders the human-readable report the user sees in the
// terminal. We split drift into three buckets (paths, api-like, symbols)
// because they call for different reactions: a bad path often means a typo
// or a missing file, a bad api-like name usually means a hallucinated
// method, and a bad symbol is the classic "invented class" case.
func printReplaySummary(dst io.Writer, rec audit.AuditRecord) error {
	w := newReportWriter(dst)
	valid, invalid := splitCitations(rec.Citations)
	short := rec.BundleHash
	if len(short) > 12 {
		short = short[:12] + "…"
	}

	w.printf("\nNeuroFS — audit replay\n\n")
	w.printf("  question     : %s\n", truncateLine(rec.Question, 70))
	w.printf("  model        : %s\n", rec.Model)
	w.printf("  bundle hash  : %s\n", short)
	w.printf("  bundle files : %d fragments\n", len(rec.Fragments))
	w.printf("\n  grounded     : %.1f%%  (%d / %d citations valid)\n",
		rec.GroundedRatio*100, len(valid), len(rec.Citations))
	w.printf("  drift rate   : %.1f%%  (%d unknown of %d referenced)\n",
		rec.Drift.Rate*100, rec.Drift.UnknownCount, rec.Drift.KnownCount+rec.Drift.UnknownCount)
	w.printf("    paths      : %d   apis : %d   symbols : %d\n",
		len(rec.Drift.UnknownPaths), len(rec.Drift.UnknownAPIs), len(rec.Drift.UnknownSymbols))
	if len(rec.ExpectsFacts) > 0 {
		w.printf("  fact recall  : %.1f%%  (%d / %d facts hit)\n",
			rec.AnswerRecall*100, len(rec.FactsHit), len(rec.ExpectsFacts))
	}

	if len(invalid) > 0 {
		w.printf("\n  invalid citations (top %d):\n", minInt(len(invalid), 5))
		for _, c := range invalid[:minInt(len(invalid), 5)] {
			w.printf("    %-40s  (%s)\n", c.Raw, c.Reason)
		}
	}
	printDriftList(w, "drift paths", rec.Drift.UnknownPaths)
	printDriftList(w, "drift apis", rec.Drift.UnknownAPIs)
	printDriftList(w, "drift symbols", rec.Drift.UnknownSymbols)
	w.printf("\n")
	return w.err
}

// printDriftList renders one bucket of drift entries if it is non-empty.
// Pulled into a helper so the three buckets share identical formatting and
// the caller reads like a table of contents.
func printDriftList(w *reportWriter, label string, items []string) {
	if len(items) == 0 {
		return
	}
	n := minInt(len(items), 5)
	w.printf("\n  %-13s (top %d):\n", label, n)
	for _, s := range items[:n] {
		w.printf("    %s\n", s)
	}
}

type reportWriter struct {
	dst io.Writer
	err error
}

func newReportWriter(dst io.Writer) *reportWriter {
	return &reportWriter{dst: dst}
}

func (w *reportWriter) printf(format string, args ...any) {
	if w.err != nil {
		return
	}
	_, w.err = fmt.Fprintf(w.dst, format, args...)
}

// splitCitations partitions a citation slice into valid and invalid halves
// while preserving their original order — we show invalid citations in the
// order the model wrote them so the user can find them in the response.
func splitCitations(cs []audit.Citation) (valid, invalid []audit.Citation) {
	for _, c := range cs {
		if c.Valid {
			valid = append(valid, c)
		} else {
			invalid = append(invalid, c)
		}
	}
	return
}

// writeJSON marshals v to path with 2-space indentation. Parent directory
// is created if missing so callers don't have to mkdir defensively.
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return atomicfile.WriteFile(path, data, 0o644)
}

func truncateLine(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

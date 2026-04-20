package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/neuromfs/neuromfs/internal/audit"
	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/packager"
	"github.com/neuromfs/neuromfs/internal/ranking"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/spf13/cobra"
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
			printDiffSummary(os.Stderr, d, args[0], args[1])

			if jsonOut != "" {
				if err := writeJSON(jsonOut, d); err != nil {
					return fmt.Errorf("audit diff: --json: %w", err)
				}
				fmt.Fprintf(os.Stderr, "  json diff  : %s\n", jsonOut)
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
func printDiffSummary(w *os.File, d audit.Diff, aPath, bPath string) {
	fmt.Fprintf(w, "\nNeuroFS — audit diff\n\n")
	fmt.Fprintf(w, "  A : %s\n", aPath)
	fmt.Fprintf(w, "  B : %s\n", bPath)

	if d.SameQuestion {
		fmt.Fprintf(w, "  question : %s\n", truncateLine(d.A.Question, 70))
	} else {
		fmt.Fprintf(w, "  question : DIFFERENT\n")
		fmt.Fprintf(w, "    A: %s\n", truncateLine(d.A.Question, 70))
		fmt.Fprintf(w, "    B: %s\n", truncateLine(d.B.Question, 70))
	}
	if d.SameModel {
		fmt.Fprintf(w, "  model    : %s\n", d.A.Model)
	} else {
		fmt.Fprintf(w, "  model    : %s → %s\n", d.A.Model, d.B.Model)
	}
	if d.SameBundle {
		fmt.Fprintf(w, "  bundle   : same (%s)\n", shortHash(d.A.BundleHash))
	} else {
		fmt.Fprintf(w, "  bundle   : DIFFERENT (%s → %s)\n",
			shortHash(d.A.BundleHash), shortHash(d.B.BundleHash))
	}

	fmt.Fprintf(w, "\n  grounded : %5.1f%% → %5.1f%%   (%+5.1f)\n",
		d.A.GroundedRatio*100, d.B.GroundedRatio*100, d.GroundedDelta*100)
	fmt.Fprintf(w, "  drift    : %5.1f%% → %5.1f%%   (%+5.1f)\n",
		d.A.Drift.Rate*100, d.B.Drift.Rate*100, d.DriftDelta*100)
	if d.RecallApplies {
		fmt.Fprintf(w, "  recall   : %5.1f%% → %5.1f%%   (%+5.1f)\n",
			d.A.AnswerRecall*100, d.B.AnswerRecall*100, d.RecallDelta*100)
	}

	printBucketDiff(w, "paths", d.Paths)
	printBucketDiff(w, "apis", d.APIs)
	printBucketDiff(w, "symbols", d.Symbols)
	fmt.Fprintln(w)
}

// printBucketDiff renders one bucket's added/removed lists, or stays silent
// when both are empty. "+" = appeared in B (potentially worse); "-" = gone
// from A (potentially better). We intentionally do not colour the output —
// terminals without ANSI support still read clearly.
func printBucketDiff(w *os.File, label string, sd audit.SetDiff) {
	if len(sd.Added) == 0 && len(sd.Removed) == 0 {
		return
	}
	fmt.Fprintf(w, "\n  %s:\n", label)
	for _, s := range sd.Added {
		fmt.Fprintf(w, "    +  %s\n", s)
	}
	for _, s := range sd.Removed {
		fmt.Fprintf(w, "    -  %s\n", s)
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
//   neurofs audit replay "<question>" --response answer.txt --repo .
//   neurofs audit replay --bundle bundle.json --response answer.txt
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

			response, err := os.ReadFile(responsePath)
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
				bundle, err = rebuildBundle(cfg, args[0], budget, focus, changedFlag, maxFiles, maxFragments)
				if err != nil {
					return fmt.Errorf("audit replay: %w", err)
				}
			}

			facts := parseFacts(factsCSV, factsFile)

			rec, err := audit.Run(context.Background(),
				audit.StubModel{Label: modelID, Response: string(response)},
				bundle,
				audit.Options{ExpectsFacts: facts},
			)
			if err != nil {
				return fmt.Errorf("audit replay: %w", err)
			}

			printReplaySummary(os.Stderr, rec)

			if jsonOut != "" {
				if err := writeJSON(jsonOut, rec); err != nil {
					return fmt.Errorf("audit replay: --json: %w", err)
				}
				fmt.Fprintf(os.Stderr, "  json record: %s\n", jsonOut)
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
				path, err := audit.SaveRecord(dir, rec)
				if err != nil {
					return fmt.Errorf("audit replay: save: %w", err)
				}
				fmt.Fprintf(os.Stderr, "  saved to   : %s\n", path)
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
	data, err := os.ReadFile(path)
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
func rebuildBundle(cfg *config.Config, query string, budget int, focus string, changedFlag bool, maxFiles, maxFragments int) (models.Bundle, error) {
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return models.Bundle{}, fmt.Errorf("open index (did you run 'neurofs scan'?): %w", err)
	}
	defer db.Close()

	files, err := db.AllFiles()
	if err != nil {
		return models.Bundle{}, err
	}

	rankOpts := ranking.Options{Project: loadProjectInfo(db), Focus: focus}
	if changedFlag {
		rankOpts.ChangedFiles = gitChangedFiles(cfg.RepoRoot)
	}
	ranked := ranking.RankWithOptions(files, query, rankOpts)

	return packager.Pack(ranked, query, packager.Options{
		Budget:           budget,
		MaxFiles:         maxFiles,
		MaxFragments:     maxFragments,
		PreferSignatures: true, // replay always treats bundles as Claude-shaped
	})
}

// parseFacts merges --facts and --facts-file. Both are optional; an empty
// result means the audit skips fact recall (ratio stays at 0, unreported).
func parseFacts(csv, file string) []string {
	var out []string
	if strings.TrimSpace(csv) != "" {
		for _, f := range strings.Split(csv, ",") {
			if s := strings.TrimSpace(f); s != "" {
				out = append(out, s)
			}
		}
	}
	if file != "" {
		data, err := os.ReadFile(file)
		if err == nil {
			for _, line := range strings.Split(string(data), "\n") {
				if s := strings.TrimSpace(line); s != "" {
					out = append(out, s)
				}
			}
		}
	}
	return out
}

// printReplaySummary renders the human-readable report the user sees in the
// terminal. We split drift into three buckets (paths, api-like, symbols)
// because they call for different reactions: a bad path often means a typo
// or a missing file, a bad api-like name usually means a hallucinated
// method, and a bad symbol is the classic "invented class" case.
func printReplaySummary(w *os.File, rec audit.AuditRecord) {
	valid, invalid := splitCitations(rec.Citations)
	short := rec.BundleHash
	if len(short) > 12 {
		short = short[:12] + "…"
	}

	fmt.Fprintf(w, "\nNeuroFS — audit replay\n\n")
	fmt.Fprintf(w, "  question     : %s\n", truncateLine(rec.Question, 70))
	fmt.Fprintf(w, "  model        : %s\n", rec.Model)
	fmt.Fprintf(w, "  bundle hash  : %s\n", short)
	fmt.Fprintf(w, "  bundle files : %d fragments\n", len(rec.Fragments))
	fmt.Fprintf(w, "\n  grounded     : %.1f%%  (%d / %d citations valid)\n",
		rec.GroundedRatio*100, len(valid), len(rec.Citations))
	fmt.Fprintf(w, "  drift rate   : %.1f%%  (%d unknown of %d referenced)\n",
		rec.Drift.Rate*100, rec.Drift.UnknownCount, rec.Drift.KnownCount+rec.Drift.UnknownCount)
	fmt.Fprintf(w, "    paths      : %d   apis : %d   symbols : %d\n",
		len(rec.Drift.UnknownPaths), len(rec.Drift.UnknownAPIs), len(rec.Drift.UnknownSymbols))
	if len(rec.ExpectsFacts) > 0 {
		fmt.Fprintf(w, "  fact recall  : %.1f%%  (%d / %d facts hit)\n",
			rec.AnswerRecall*100, len(rec.FactsHit), len(rec.ExpectsFacts))
	}

	if len(invalid) > 0 {
		fmt.Fprintf(w, "\n  invalid citations (top %d):\n", minInt(len(invalid), 5))
		for _, c := range invalid[:minInt(len(invalid), 5)] {
			fmt.Fprintf(w, "    %-40s  (%s)\n", c.Raw, c.Reason)
		}
	}
	printDriftList(w, "drift paths", rec.Drift.UnknownPaths)
	printDriftList(w, "drift apis", rec.Drift.UnknownAPIs)
	printDriftList(w, "drift symbols", rec.Drift.UnknownSymbols)
	fmt.Fprintln(w)
}

// printDriftList renders one bucket of drift entries if it is non-empty.
// Pulled into a helper so the three buckets share identical formatting and
// the caller reads like a table of contents.
func printDriftList(w *os.File, label string, items []string) {
	if len(items) == 0 {
		return
	}
	n := minInt(len(items), 5)
	fmt.Fprintf(w, "\n  %-13s (top %d):\n", label, n)
	for _, s := range items[:n] {
		fmt.Fprintf(w, "    %s\n", s)
	}
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
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
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


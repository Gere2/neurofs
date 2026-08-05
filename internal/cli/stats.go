package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"time"

	"github.com/Gere2/neurofs/internal/audit"
	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/grounding"
	"github.com/Gere2/neurofs/internal/indexer"
	"github.com/Gere2/neurofs/internal/models"
	"github.com/Gere2/neurofs/internal/project"
	"github.com/Gere2/neurofs/internal/storage"
	"github.com/spf13/cobra"
)

func newStatsCmd() *cobra.Command {
	var (
		repoPath string
		top      int
	)

	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Show metrics about the current index",
		Long: `Stats reports file count, language breakdown, symbol/import totals,
index size on disk, and the top files by symbol density.

Use this to audit what 'scan' actually captured — if your repo has 200 files
but only 40 are indexed, something is being filtered out.`,
		RunE: func(cmd *cobra.Command, args []string) (retErr error) {
			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("stats: %w", err)
			}
			if err := cfg.Validate(); err != nil {
				return fmt.Errorf("stats: config: %w", err)
			}

			db, err := storage.Open(cfg.DBPath)
			if err != nil {
				return fmt.Errorf("stats: open index (did you run 'neurofs scan'?): %w", err)
			}
			defer func() {
				if err := db.Close(); err != nil {
					retErr = errors.Join(retErr, fmt.Errorf("stats: close index: %w", err))
				}
			}()

			count, err := db.FileCount()
			if err != nil {
				return fmt.Errorf("stats: file count: %w", err)
			}
			if count == 0 {
				return fmt.Errorf("stats: index is empty — run 'neurofs scan' first")
			}

			breakdown, err := db.LangBreakdown()
			if err != nil {
				return fmt.Errorf("stats: lang breakdown: %w", err)
			}
			totalBytes, err := db.TotalBytes()
			if err != nil {
				return fmt.Errorf("stats: total bytes: %w", err)
			}
			dbBytes, err := db.DBSize()
			if err != nil {
				return fmt.Errorf("stats: db size: %w", err)
			}
			lastScan, err := db.LastIndexedAt()
			if err != nil {
				return fmt.Errorf("stats: last scan: %w", err)
			}
			files, err := db.AllFiles()
			if err != nil {
				return fmt.Errorf("stats: load files: %w", err)
			}

			totalSymbols := 0
			totalImports := 0
			for _, f := range files {
				totalSymbols += len(f.Symbols)
				totalImports += len(f.Imports)
			}

			out := newReportWriter(cmd.OutOrStdout())
			out.printf("NeuroFS — index at %s\n\n", cfg.DBPath)
			out.printf("  repo root  : %s\n", cfg.RepoRoot)
			out.printf("  files      : %d indexed\n", count)
			out.printf("  symbols    : %d total\n", totalSymbols)
			out.printf("  imports    : %d total\n", totalImports)
			out.printf("  source     : %s\n", humanBytes(totalBytes))
			out.printf("  index size : %s\n", humanBytes(dbBytes))
			if !lastScan.IsZero() {
				out.printf("  last scan  : %s (%s)\n",
					lastScan.Local().Format("2006-01-02 15:04:05"),
					humanSince(time.Since(lastScan)),
				)
			}

			if raw, ok, _ := db.GetMeta(indexer.ProjectMetaKey); ok && raw != "" {
				if info := project.Decode(raw); info != nil {
					out.printf("\n  project    : %s\n", info.Label())
					if len(info.Dependencies) > 0 || len(info.DevDependencies) > 0 {
						out.printf("  deps       : %d production / %d dev\n",
							len(info.Dependencies), len(info.DevDependencies))
					}
					if len(info.PathAliases) > 0 {
						out.printf("  ts aliases : %d\n", len(info.PathAliases))
					}
					if entries := info.EntryPoints(); len(entries) > 0 {
						out.printf("  entry pts  : %s\n", joinStrings(entries, ", "))
					}
				}
			}

			out.printf("\n  by language:\n")
			printLangBreakdown(out, breakdown)

			if top > 0 {
				out.printf("\n  top %d by symbol count:\n", top)
				printTopBySymbols(out, files, top)
			}
			if out.err != nil {
				return fmt.Errorf("stats: write summary: %w", out.err)
			}

			if err := printAuditSummary(out.dst, filepath.Join(cfg.RepoRoot, audit.DefaultRecordsDir)); err != nil {
				return fmt.Errorf("stats: write audit summary: %w", err)
			}
			if err := printGroundingSummary(out.dst, cfg.RepoRoot); err != nil {
				return fmt.Errorf("stats: grounding summary: %w", err)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository root (defaults to current directory)")
	cmd.Flags().IntVar(&top, "top", 10, "Number of files to show in the top-by-symbols list (0 to disable)")
	return cmd
}

func printLangBreakdown(w *reportWriter, breakdown map[models.Lang]int) {
	type entry struct {
		lang  models.Lang
		count int
	}
	entries := make([]entry, 0, len(breakdown))
	for lang, n := range breakdown {
		entries = append(entries, entry{lang, n})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].count > entries[j].count })
	for _, e := range entries {
		w.printf("    %-12s : %d\n", e.lang, e.count)
	}
}

func printTopBySymbols(w *reportWriter, files []models.FileRecord, top int) {
	sorted := make([]models.FileRecord, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool {
		return len(sorted[i].Symbols) > len(sorted[j].Symbols)
	})
	if top > len(sorted) {
		top = len(sorted)
	}
	for _, f := range sorted[:top] {
		if len(f.Symbols) == 0 {
			break
		}
		w.printf("    %-40s : %d symbols\n", f.RelPath, len(f.Symbols))
	}
}

// printAuditSummary scans recordsDir and renders an aggregate governance
// block. A missing or empty directory is a legitimate state — we stay silent
// in that case so the section only appears when there's something to report.
func printAuditSummary(dst io.Writer, recordsDir string) error {
	paths, err := audit.ListRecords(recordsDir)
	if err != nil || len(paths) == 0 {
		return nil
	}
	records := make([]audit.AuditRecord, 0, len(paths))
	for _, p := range paths {
		rec, err := audit.LoadRecord(p)
		if err != nil {
			continue
		}
		records = append(records, rec)
	}
	if len(records) == 0 {
		return nil
	}
	w := newReportWriter(dst)
	agg := audit.AggregateFrom(records)
	w.printf("\n  audit records : %d replayed\n", agg.Records)
	w.printf("    grounded    : %.1f%%\n", agg.GroundedRatio*100)
	w.printf("    drift       : %.1f%%\n", agg.DriftRate*100)
	if agg.AnswerRecall > 0 {
		w.printf("    fact recall : %.1f%%\n", agg.AnswerRecall*100)
	}
	if len(agg.Models) > 0 {
		type mc struct {
			name  string
			count int
		}
		ms := make([]mc, 0, len(agg.Models))
		for name, c := range agg.Models {
			ms = append(ms, mc{name, c})
		}
		sort.Slice(ms, func(i, j int) bool { return ms[i].count > ms[j].count })
		w.printf("    by model    :")
		for i, m := range ms {
			sep := ","
			if i == len(ms)-1 {
				sep = ""
			}
			w.printf(" %s=%d%s", m.name, m.count, sep)
		}
		w.printf("\n")
	}
	return w.err
}

// printGroundingSummary renders the continuous-grounding rollup if the loop
// has accumulated any events. Silent when the ledger is empty — the section
// only appears once `neurofs ground` has recorded something.
func printGroundingSummary(dst io.Writer, repoRoot string) error {
	events, err := grounding.Read(repoRoot)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	w := newReportWriter(dst)
	agg := grounding.Summarize(events)
	w.printf("\n  grounding feed : %d events\n", agg.Events)
	if agg.Edits > 0 {
		w.printf("    edits in ctx : %.0f%% (%d/%d)\n", agg.EditCoverage*100, agg.EditsInContext, agg.Edits)
	}
	if agg.Responses > 0 {
		w.printf("    resp grounded: %.0f%% (drift %.0f%%)\n", agg.MeanGroundedResp*100, agg.MeanRespDrift*100)
	}
	w.printf("    concerning   : %d — see `neurofs ground --feed`\n", agg.Concerning)
	return w.err
}

// humanBytes renders a byte count as a compact human-readable string.
func humanBytes(n int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case n < kb:
		return fmt.Sprintf("%d B", n)
	case n < mb:
		return fmt.Sprintf("%.1f KB", float64(n)/kb)
	case n < gb:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/gb)
	}
}

// joinStrings concatenates ss with sep; defined locally to avoid pulling in
// strings just for one callsite.
func joinStrings(ss []string, sep string) string {
	switch len(ss) {
	case 0:
		return ""
	case 1:
		return ss[0]
	}
	out := ss[0]
	for _, s := range ss[1:] {
		out += sep + s
	}
	return out
}

// humanSince renders a duration as "2 minutes ago", "3 hours ago", etc.
func humanSince(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d minutes ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours())/24)
	}
}

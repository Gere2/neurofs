package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/indexer"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/neuromfs/neuromfs/internal/taskflow"
)

const taskInputSchema = `{
  "type": "object",
  "properties": {
    "query":  { "type": "string",  "description": "What you're trying to do (a sentence)." },
    "repo":   { "type": "string",  "description": "Absolute path to repo. Default: cwd." },
    "budget": { "type": "integer", "description": "Token budget. Default: 3000." }
  },
  "required": ["query"]
}`

const scanInputSchema = `{
  "type": "object",
  "properties": {
    "repo": { "type": "string", "description": "Absolute path. Default: cwd." }
  }
}`

func toolsList() []Tool {
	return []Tool{
		{
			Name:        "neurofs_task",
			Description: "Pack a Claude-ready prompt for a given intention against a repo. Returns the prompt text.",
			InputSchema: json.RawMessage(taskInputSchema),
		},
		{
			Name:        "neurofs_scan",
			Description: "Index a repo and return a read-only summary (file count, total size, top extensions).",
			InputSchema: json.RawMessage(scanInputSchema),
		},
	}
}

func callTool(ctx context.Context, p ToolCallParams) ToolCallResult {
	switch p.Name {
	case "neurofs_task":
		return runTaskTool(ctx, p.Arguments)
	case "neurofs_scan":
		return runScanTool(ctx, p.Arguments)
	default:
		return errResult(fmt.Sprintf("unknown tool: %q", p.Name))
	}
}

type taskArgs struct {
	Query  string `json:"query"`
	Repo   string `json:"repo"`
	Budget int    `json:"budget"`
}

func runTaskTool(_ context.Context, raw json.RawMessage) ToolCallResult {
	var args taskArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err))
		}
	}
	args.Query = strings.TrimSpace(args.Query)
	if args.Query == "" {
		return errResult("query must not be empty")
	}
	if args.Budget <= 0 {
		args.Budget = 3000
	}
	repo, err := resolveRepo(args.Repo)
	if err != nil {
		return errResult(err.Error())
	}

	result, err := taskflow.Run(taskflow.Opts{
		RepoRoot: repo,
		Query:    args.Query,
		Budget:   args.Budget,
	})
	if err != nil {
		return errResult(err.Error())
	}
	return textResult(result.Prompt)
}

type scanArgs struct {
	Repo string `json:"repo"`
}

func runScanTool(_ context.Context, raw json.RawMessage) ToolCallResult {
	var args scanArgs
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &args); err != nil {
			return errResult(fmt.Sprintf("invalid arguments: %v", err))
		}
	}
	repo, err := resolveRepo(args.Repo)
	if err != nil {
		return errResult(err.Error())
	}

	cfg, err := config.New(repo)
	if err != nil {
		return errResult(err.Error())
	}
	if err := cfg.Validate(); err != nil {
		return errResult(err.Error())
	}

	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return errResult(err.Error())
	}
	defer db.Close()

	stats, err := indexer.Run(cfg, db, indexer.Options{Logf: func(string, ...any) {}})
	if err != nil {
		return errResult(err.Error())
	}

	files, err := db.AllFiles()
	if err != nil {
		return errResult(err.Error())
	}

	var totalSize int64
	extCount := make(map[string]int)
	extBytes := make(map[string]int64)
	for _, f := range files {
		totalSize += f.Size
		ext := strings.ToLower(filepath.Ext(f.RelPath))
		if ext == "" {
			ext = "(none)"
		}
		extCount[ext]++
		extBytes[ext] += f.Size
	}

	type extRow struct {
		ext   string
		count int
		bytes int64
	}
	rows := make([]extRow, 0, len(extCount))
	for k, v := range extCount {
		rows = append(rows, extRow{ext: k, count: v, bytes: extBytes[k]})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].count != rows[j].count {
			return rows[i].count > rows[j].count
		}
		return rows[i].ext < rows[j].ext
	})

	var b strings.Builder
	fmt.Fprintf(&b, "neurofs scan — %s\n", cfg.RepoRoot)
	fmt.Fprintf(&b, "  files indexed : %d\n", stats.Indexed)
	fmt.Fprintf(&b, "  discovered    : %d\n", stats.Discovered)
	fmt.Fprintf(&b, "  skipped       : %d\n", stats.Skipped)
	fmt.Fprintf(&b, "  symbols       : %d\n", stats.Symbols)
	fmt.Fprintf(&b, "  imports       : %d\n", stats.Imports)
	fmt.Fprintf(&b, "  total size    : %s\n", humanBytes(totalSize))
	fmt.Fprintf(&b, "  index db      : %s\n", cfg.DBPath)
	fmt.Fprintf(&b, "\n  top extensions:\n")
	limit := len(rows)
	if limit > 10 {
		limit = 10
	}
	for i := 0; i < limit; i++ {
		fmt.Fprintf(&b, "    %-10s %5d files  %s\n", rows[i].ext, rows[i].count, humanBytes(rows[i].bytes))
	}
	return textResult(b.String())
}

func resolveRepo(path string) (string, error) {
	if path != "" {
		return path, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("resolve cwd: %w", err)
	}
	return cwd, nil
}

func textResult(text string) ToolCallResult {
	return ToolCallResult{Content: []Content{{Type: "text", Text: text}}}
}

func errResult(msg string) ToolCallResult {
	return ToolCallResult{
		Content: []Content{{Type: "text", Text: msg}},
		IsError: true,
	}
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

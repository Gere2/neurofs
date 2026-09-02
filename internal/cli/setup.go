package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Gere2/neurofs/internal/config"
	"github.com/Gere2/neurofs/internal/taskflow"
	"github.com/spf13/cobra"
)

// claudeMDMarker detects a repo that is already set up: it is the one tool
// name every retrieval instruction block must mention, however the user
// has since rephrased the surrounding prose.
const claudeMDMarker = "neurofs_context"

// claudeMDBlock is the agent instruction block setup appends. It is the
// contract that makes the learn loop self-feeding: neurofs first for
// retrieval, one feedback call per task afterwards.
const claudeMDBlock = `
## Retrieval (NeuroFS)

- Before reading whole files, ask NeuroFS first: use ` + "`neurofs_context`" + `
  (or ` + "`neurofs_search`" + `) to get targeted, citable excerpts.
- Preserve the returned ` + "`session_id`" + ` across follow-up calls. Pass the
  returned ` + "`retrieval_id`" + ` as ` + "`parent_retrieval_id`" + ` when an expansion or
  another retrieval follows from it.
- After finishing a task that used those results, call ` + "`neurofs_feedback`" + `
  once with that exact ` + "`retrieval_id`" + `: rating ` + "`yes`/`no`/`partial`" + `,
  the symbols/paths that actually helped, and any identifier that should have
  been retrieved but wasn't. Only name symbols you verified exist.
`

// newSetupCmd wires a repo for the improve-through-use loop in one shot:
// index it, add the CLAUDE.md retrieval contract, and print the MCP
// registration for this binary. Everything is idempotent so re-running
// after a rename or move is safe.
func newSetupCmd() *cobra.Command {
	var repoPath string

	cmd := &cobra.Command{
		Use:   "setup [path]",
		Short: "Prepare a repository for agent use: index it, wire CLAUDE.md, print MCP registration",
		Long: `Setup performs the per-repository adoption steps in one command:

  1. Index the repo (skipped when a fresh index already exists).
  2. Append the retrieval instruction block to CLAUDE.md (created if
     missing, skipped if the block is already there) — this is what makes
     agents use NeuroFS and report feedback, which feeds 'neurofs learn'.
  3. Print the one-time MCP registration command for Claude Code.

Run it from any repo you want agents to work in:

  cd /path/to/project && neurofs setup`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				repoPath = args[0]
			}
			cfg, err := config.New(repoPath)
			if err != nil {
				return fmt.Errorf("setup: %w", err)
			}

			// 1. Index.
			if indexReady(cfg.DBPath) {
				fmt.Printf("  index     : ok (%s)\n", cfg.DBPath)
			} else {
				fmt.Printf("  index     : building…\n")
				if err := taskflow.EnsureFreshIndex(cfg); err != nil {
					return fmt.Errorf("setup: scan: %w", err)
				}
				fmt.Printf("  index     : built (%s)\n", cfg.DBPath)
			}

			// 2. CLAUDE.md contract.
			status, err := wireClaudeMD(cfg.RepoRoot)
			if err != nil {
				return fmt.Errorf("setup: CLAUDE.md: %w", err)
			}
			switch status {
			case "already":
				fmt.Printf("  CLAUDE.md : already wired (mentions %s)\n", claudeMDMarker)
			case "appended":
				fmt.Printf("  CLAUDE.md : retrieval block appended\n")
			case "created":
				fmt.Printf("  CLAUDE.md : created with the retrieval block\n")
			}

			// 3. MCP registration hint (one-time, global for every repo).
			exe, err := os.Executable()
			if err != nil {
				exe = "neurofs"
			}
			fmt.Printf("\nIf NeuroFS is not yet registered with Claude Code (one time, all repos):\n")
			fmt.Printf("  claude mcp add --scope user neurofs -- %s mcp\n", exe)
			fmt.Printf("\nOptional while working here:\n")
			fmt.Printf("  neurofs watch          # keep the index fresh\n")
			fmt.Printf("  neurofs learn status   # accumulated usage/feedback signal\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&repoPath, "repo", "", "Repository path (default: cwd)")
	return cmd
}

func wireClaudeMD(repoRoot string) (string, error) {
	claudePath := filepath.Join(repoRoot, "CLAUDE.md")
	info, err := regularFileInfo(claudePath)
	switch {
	case err == nil:
		existing, readErr := os.ReadFile(claudePath)
		if readErr != nil {
			return "", readErr
		}
		if strings.Contains(string(existing), claudeMDMarker) {
			return "already", nil
		}
		if writeErr := atomicWriteFile(claudePath, append(existing, []byte(claudeMDBlock)...), info.Mode().Perm()); writeErr != nil {
			return "", writeErr
		}
		return "appended", nil
	case os.IsNotExist(err):
		content := "# " + filepath.Base(repoRoot) + "\n" + claudeMDBlock
		if writeErr := atomicWriteFile(claudePath, []byte(content), 0o644); writeErr != nil {
			return "", writeErr
		}
		return "created", nil
	default:
		return "", err
	}
}

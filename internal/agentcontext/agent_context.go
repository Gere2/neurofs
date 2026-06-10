// Package agentcontext builds patch-oriented context for coding agents.
package agentcontext

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/neuromfs/neuromfs/internal/config"
	"github.com/neuromfs/neuromfs/internal/contextmap"
	"github.com/neuromfs/neuromfs/internal/models"
	"github.com/neuromfs/neuromfs/internal/storage"
	"github.com/neuromfs/neuromfs/internal/taskflow"
	"github.com/neuromfs/neuromfs/internal/tokenbudget"
)

// Transport controls how expansion instructions are rendered.
type Transport string

const (
	TransportCLI Transport = "cli"
	TransportMCP Transport = "mcp"
)

// Options tunes patch-context rendering.
type Options struct {
	Transport Transport
	Thin      bool
}

// PatchPrompt is a task prompt plus patch-context measurement metadata.
type PatchPrompt struct {
	Text           string
	InitialTokens  int
	BaselineTokens int
	NextActions    []NextAction
	Thin           bool
}

// NextAction is a structured MCP step an agent can take after reading the
// initial patch context.
type NextAction struct {
	Tool   string         `json:"tool"`
	Input  map[string]any `json:"input"`
	Reason string         `json:"reason,omitempty"`
}

// BuildPatchPrompt appends editable ranges, logic maps, invariants, and
// measurement instructions to a taskflow prompt.
func BuildPatchPrompt(repoRoot, sessionID string, result taskflow.Result, opts Options) (PatchPrompt, error) {
	transport := opts.Transport
	if transport == "" {
		transport = TransportCLI
	}

	cfg, err := config.New(repoRoot)
	if err != nil {
		return PatchPrompt{}, err
	}
	db, err := storage.Open(cfg.DBPath)
	if err != nil {
		return PatchPrompt{}, err
	}
	defer db.Close()

	files, err := db.AllFiles()
	if err != nil {
		return PatchPrompt{}, err
	}
	rels, _ := db.AllRelations()
	records := make(map[string]models.FileRecord, len(files))
	for _, rec := range files {
		records[rec.RelPath] = rec
	}

	basePrompt := result.Prompt
	if opts.Thin {
		basePrompt = buildThinPrompt(result)
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "\n\n<patch_context session=%q>\n", sessionID)
	writeContextLadder(&sb, result.Query, sessionID, transport)

	seen := make(map[string]bool)
	probableTests := make(map[string]bool)
	var baselineTokens int
	var nextActions []NextAction

	fmt.Fprintf(&sb, "<editable_fragments>\n")
	for _, frag := range result.Bundle.Fragments {
		rec, ok := records[frag.RelPath]
		if !ok {
			continue
		}
		if !seen[rec.RelPath] {
			contentBytes, err := os.ReadFile(rec.Path)
			if err == nil {
				baselineTokens += tokenbudget.EstimateTokens(string(contentBytes))
			}
			seen[rec.RelPath] = true
		}
		fmt.Fprintf(&sb, "<fragment path=%q rep=%q tokens=%d", frag.RelPath, frag.Representation, frag.Tokens)
		if frag.StartLine > 0 && frag.EndLine >= frag.StartLine {
			fmt.Fprintf(&sb, " start=%d end=%d", frag.StartLine, frag.EndLine)
		}
		if frag.ContentHash != "" {
			fmt.Fprintf(&sb, " hash=%q", frag.ContentHash)
		}
		fmt.Fprintf(&sb, ">\n")
		if frag.StartLine > 0 && frag.EndLine >= frag.StartLine {
			nextActions = addNextAction(nextActions, expandRangeAction(frag.RelPath, frag.StartLine, frag.EndLine, frag.ContentHash, sessionID, "inspect the selected editable range"))
			fmt.Fprintf(&sb, "expand: %s\n", expandRangeInstruction(frag.RelPath, frag.StartLine, frag.EndLine, frag.ContentHash, sessionID, transport))
		} else {
			nextActions = addNextAction(nextActions, outlineAction(frag.RelPath, "inspect symbols, imports, and related tests before expanding content"))
			fmt.Fprintf(&sb, "outline: %s\n", outlineInstruction(frag.RelPath, sessionID, transport))
			nextActions = addNextAction(nextActions, fullAction(frag.RelPath, sessionID, "read the whole file only if outline and excerpts are insufficient"))
			fmt.Fprintf(&sb, "full: %s\n", fullInstruction(frag.RelPath, sessionID, transport))
		}
		fmt.Fprintf(&sb, "</fragment>\n")
	}
	fmt.Fprintf(&sb, "</editable_fragments>\n\n")

	fmt.Fprintf(&sb, "<logic_maps>\n")
	for relPath := range seen {
		rec := records[relPath]
		chunks, _ := db.GetChunksForFile(rec.Path)
		contentBytes, _ := os.ReadFile(rec.Path)
		logic := contextmap.Build(rec, files, chunks, rels, string(contentBytes))
		for _, test := range logic.RelatedTests {
			probableTests[test] = true
		}
		writeAgentLogicMap(&sb, logic, sessionID, transport)
	}
	fmt.Fprintf(&sb, "</logic_maps>\n\n")

	fmt.Fprintf(&sb, "<invariants>\n")
	fmt.Fprintf(&sb, "- Treat visible fragments as editable only inside their shown ranges unless expanded.\n")
	fmt.Fprintf(&sb, "- If a required body, dependency, or test is not visible, expand the smallest outline or excerpt first.\n")
	fmt.Fprintf(&sb, "- Prefer changing production code and related tests together when the logic map names tests.\n")
	fmt.Fprintf(&sb, "- Preserve public symbols, imports, and call relationships unless the task explicitly changes them.\n")
	fmt.Fprintf(&sb, "</invariants>\n\n")

	if len(probableTests) > 0 {
		fmt.Fprintf(&sb, "<probable_tests>\n")
		for _, test := range sortedKeys(probableTests) {
			fmt.Fprintf(&sb, "- %s\n", test)
			nextActions = addNextAction(nextActions, outlineAction(test, "inspect related test before changing behavior"))
		}
		fmt.Fprintf(&sb, "</probable_tests>\n\n")
	}
	nextActions = addNextAction(nextActions, NextAction{
		Tool:   "neurofs_measure",
		Input:  map[string]any{"session_id": sessionID},
		Reason: "summarize actual context tokens after expansions",
	})
	if transport == TransportMCP && len(nextActions) > 0 {
		fmt.Fprintf(&sb, "<next_actions>\n")
		for _, action := range nextActions {
			fmt.Fprintf(&sb, "- %s\n", mcpToolCall(action.Tool, action.Input))
		}
		fmt.Fprintf(&sb, "</next_actions>\n\n")
	}

	initialEstimate := tokenbudget.EstimateTokens(basePrompt + sb.String())
	fmt.Fprintf(&sb, "<measurement session=%q initial_tokens=%d eager_full_file_tokens=%d>\n",
		sessionID, initialEstimate, baselineTokens)
	fmt.Fprintf(&sb, "summary: %s\n", measureInstruction(sessionID, transport))
	fmt.Fprintf(&sb, "</measurement>\n")
	fmt.Fprintf(&sb, "</patch_context>\n")

	text := basePrompt + sb.String()
	return PatchPrompt{
		Text:           text,
		InitialTokens:  tokenbudget.EstimateTokens(text),
		BaselineTokens: baselineTokens,
		NextActions:    nextActions,
		Thin:           opts.Thin,
	}, nil
}

func buildThinPrompt(result taskflow.Result) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "<task repo=%q budget=%d chunk_mode=%t>\n", result.RepoRoot, result.Budget, result.ChunkMode)
	fmt.Fprintf(&sb, "%s\n", result.Query)
	fmt.Fprintf(&sb, "</task>\n\n")
	fmt.Fprintf(&sb, "<selected_context>\n")
	for _, frag := range result.Bundle.Fragments {
		fmt.Fprintf(&sb, "- path=%q rep=%q tokens=%d", frag.RelPath, frag.Representation, frag.Tokens)
		if frag.StartLine > 0 && frag.EndLine >= frag.StartLine {
			fmt.Fprintf(&sb, " range=%d-%d", frag.StartLine, frag.EndLine)
		}
		if frag.ContentHash != "" {
			fmt.Fprintf(&sb, " hash=%s", frag.ContentHash)
		}
		fmt.Fprintln(&sb)
	}
	fmt.Fprintf(&sb, "</selected_context>\n")
	return sb.String()
}

func writeContextLadder(w *strings.Builder, query, sessionID string, transport Transport) {
	fmt.Fprintf(w, "<context_ladder>\n")
	switch transport {
	case TransportMCP:
		fmt.Fprintf(w, "1. search: call %s\n", mcpToolCall("neurofs_search", map[string]any{"query": query, "mode": "build"}))
		fmt.Fprintf(w, "2. outline: call %s\n", mcpToolCall("neurofs_get_outline", map[string]any{"path": "<path>"}))
		fmt.Fprintf(w, "3. excerpt: call %s\n", mcpToolCall("neurofs_expand", map[string]any{"target": "<path:start-end>", "hash": "<hash>", "session_id": sessionID}))
		fmt.Fprintf(w, "4. full file: call %s\n", mcpToolCall("neurofs_expand", map[string]any{"target": "<path>", "mode": "full", "session_id": sessionID}))
	default:
		fmt.Fprintf(w, "1. files-only: neurofs task %q --files-only --json --no-embeddings\n", query)
		fmt.Fprintf(w, "2. outline: neurofs expand <path> --session %s\n", sessionID)
		fmt.Fprintf(w, "3. excerpt: neurofs expand <path:start-end> --hash <hash> --session %s\n", sessionID)
		fmt.Fprintf(w, "4. full file: neurofs expand <path> --mode full --session %s\n", sessionID)
	}
	fmt.Fprintf(w, "</context_ladder>\n\n")
}

func writeAgentLogicMap(w *strings.Builder, logic contextmap.LogicMap, sessionID string, transport Transport) {
	fmt.Fprintf(w, "<logic path=%q lang=%q lines=%d>\n", logic.Path, logic.Lang, logic.Lines)
	if len(logic.Symbols) > 0 {
		fmt.Fprintf(w, "symbols:\n")
		limit := len(logic.Symbols)
		if limit > 8 {
			limit = 8
		}
		for _, sym := range logic.Symbols[:limit] {
			fmt.Fprintf(w, "- %s %s L%d-L%d", sym.Kind, sym.Name, sym.StartLine, sym.EndLine)
			if sym.ContentHash != "" {
				fmt.Fprintf(w, " hash=%s", sym.ContentHash)
			}
			if len(sym.Calls) > 0 {
				fmt.Fprintf(w, " calls=%s", strings.Join(firstStrings(sym.Calls, 5), ","))
			}
			fmt.Fprintln(w)
		}
		if len(logic.Symbols) > limit {
			fmt.Fprintf(w, "- +%d more symbols; use `%s` for full outline\n",
				len(logic.Symbols)-limit, outlineInstruction(logic.Path, sessionID, transport))
		}
	}
	writeAgentList(w, "dependencies", firstStrings(logic.Dependencies, 6))
	writeAgentList(w, "dependents", firstStrings(logic.Dependents, 6))
	writeAgentList(w, "related_tests", firstStrings(logic.RelatedTests, 6))
	fmt.Fprintf(w, "</logic>\n")
}

func writeAgentList(w *strings.Builder, name string, values []string) {
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(w, "%s:\n", name)
	for _, value := range values {
		fmt.Fprintf(w, "- %s\n", value)
	}
}

func expandRangeInstruction(path string, startLine, endLine int, hash, sessionID string, transport Transport) string {
	target := fmt.Sprintf("%s:%d-%d", path, startLine, endLine)
	if transport == TransportMCP {
		action := expandRangeAction(path, startLine, endLine, hash, sessionID, "")
		return "call " + mcpToolCall(action.Tool, action.Input)
	}
	hashArg := ""
	if hash != "" {
		hashArg = " --hash " + hash
	}
	return fmt.Sprintf("neurofs expand %s%s --session %s", target, hashArg, sessionID)
}

func outlineInstruction(path, sessionID string, transport Transport) string {
	if transport == TransportMCP {
		action := outlineAction(path, "")
		return "call " + mcpToolCall(action.Tool, action.Input)
	}
	return fmt.Sprintf("neurofs expand %s --session %s", path, sessionID)
}

func fullInstruction(path, sessionID string, transport Transport) string {
	if transport == TransportMCP {
		action := fullAction(path, sessionID, "")
		return "call " + mcpToolCall(action.Tool, action.Input)
	}
	return fmt.Sprintf("neurofs expand %s --mode full --session %s", path, sessionID)
}

func measureInstruction(sessionID string, transport Transport) string {
	if transport == TransportMCP {
		return "call " + mcpToolCall("neurofs_measure", map[string]any{"session_id": sessionID})
	}
	return fmt.Sprintf("neurofs measure --session %s", sessionID)
}

func expandRangeAction(path string, startLine, endLine int, hash, sessionID, reason string) NextAction {
	target := fmt.Sprintf("%s:%d-%d", path, startLine, endLine)
	input := map[string]any{"target": target, "session_id": sessionID}
	if hash != "" {
		input["hash"] = hash
	}
	return NextAction{Tool: "neurofs_expand", Input: input, Reason: reason}
}

func outlineAction(path, reason string) NextAction {
	return NextAction{
		Tool:   "neurofs_get_outline",
		Input:  map[string]any{"path": path},
		Reason: reason,
	}
}

func fullAction(path, sessionID, reason string) NextAction {
	return NextAction{
		Tool:   "neurofs_expand",
		Input:  map[string]any{"target": path, "mode": "full", "session_id": sessionID},
		Reason: reason,
	}
}

func addNextAction(actions []NextAction, action NextAction) []NextAction {
	if action.Tool == "" {
		return actions
	}
	key := action.Tool + "\x00" + stableActionInput(action.Input)
	for _, existing := range actions {
		if existing.Tool+"\x00"+stableActionInput(existing.Input) == key {
			return actions
		}
	}
	return append(actions, action)
}

func stableActionInput(input map[string]any) string {
	payload, err := json.Marshal(input)
	if err != nil {
		return fmt.Sprintf("%v", input)
	}
	return string(payload)
}

func mcpToolCall(name string, args map[string]any) string {
	payload, err := json.Marshal(args)
	if err != nil {
		return name + " {}"
	}
	return fmt.Sprintf("%s %s", name, payload)
}

func sortedKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func firstStrings(values []string, limit int) []string {
	if limit <= 0 || len(values) <= limit {
		return values
	}
	return values[:limit]
}

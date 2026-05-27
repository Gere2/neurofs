# NeuroFS

**A context compiler for LLMs.**

NeuroFS does not try to be another coding copilot.  
It solves a different and more upstream problem:

> Given a question about a repository, produce better context for an LLM than copy-paste or naive retrieval.

---

## The Problem

Most AI coding workflows fail not because the model is weak, but because the input is poor.

Three issues repeat:

1. **Too much irrelevant context.** Noise drowns signal.
2. **Missing structural relationships.** Files and symbols are disconnected in the prompt.
3. **Manual selection does not scale.** Developers waste time deciding what to paste.

Result: garbage in, garbage out.

---

## What NeuroFS Does

Given a question about a codebase, NeuroFS:

1. **Indexes** repository structure and symbols
2. **Ranks** files by relevance using structural and lexical signals
3. **Expands** through import relationships to capture dependency clusters
4. **Selects** a representation for each fragment: `full_code`, `signature`, `structural_note`
5. **Packages** the minimum necessary context within a token budget
6. **Justifies** every inclusion — no opaque retrieval

The output is a self-contained, auditable bundle ready for any LLM interface.

---

## Quickstart

```bash
# Build the binary
make build

# Shortest path: one-shot prompt from a question, auto-scan included
./bin/neurofs task "where is jwt verified" | pbcopy

# Or open the local UI (loopback only — nothing leaves your machine)
./bin/neurofs ui

# Or keep the SQLite index fresh while you work
./bin/neurofs watch

# Or expose neurofs as MCP tools to Claude Desktop / Cursor / any MCP host
./bin/neurofs mcp     # stdio JSON-RPC — wire it as an MCP server
```

`neurofs task` writes a paste-ready prompt to stdout and the summary
(tokens, files, top picks, cache status) to stderr — composes as a Unix
filter. The UI wraps the same flow plus `scan`, `pack`, `replay`, the
journal, and global search in one page; it opens at
<http://127.0.0.1:7777> automatically. `neurofs mcp` exposes the same
flow as MCP tools so a host LLM can call NeuroFS directly. The lower-level
`neurofs ask` / `neurofs pack` commands stay available — see
[Commands](#commands).

---

## Installation

**Requirements:** Go 1.22+

```bash
# From source
git clone https://github.com/neuromfs/neuromfs
cd neuromfs
make install   # installs to $GOPATH/bin

# Direct installation (requires Go 1.22+)
go install github.com/neuromfs/neuromfs/cmd/neurofs@latest
```

---

## Commands

### `neurofs task <query>`

The shortest path from a repository and a question to a paste-ready
prompt. Auto-scans on first use, caches by `(query, budget)`, and writes
the prompt to stdout — pipe it, redirect it, or let your shell's
clipboard helper handle it.

```
neurofs task "where is jwt verified"               # prints prompt to stdout
neurofs task "review my ranking changes" > p.md    # redirect to a file
neurofs task "resume seed UI" --budget 3000        # tighter budget
neurofs task "where is BuildChunks" --no-chunks    # build from ranked whole files instead of chunks
neurofs task "..." --force                         # ignore the cache
```

Stderr carries a short summary (tokens, files, top picks, cache
status, mode) so pipes stay clean. By default, it uses chunk-based retrieval;
pass `--no-chunks` to fall back to whole files. It uses the same hybrid
retrieval path as `neurofs_search` and emits excerpt fragments with
line ranges instead of whole-file ranked fragments. Shared logic lives
in `internal/taskflow`, so `neurofs task` and the UI's Task tab can
never drift.

### `neurofs scan [path]`

Indexes a repository and writes the result to `.neurofs/index.db` inside the repo.

```
neurofs scan                      # scans current directory
neurofs scan /path/to/repo        # scans a specific path
neurofs scan /path/to/repo -v     # verbose (logs each file)
```

Output:
```
NeuroFS — scanning /your/repo

  discovered : 47 files
  indexed    : 32 files
  skipped    : 15 files
  symbols    : 218
  imports    : 89
  index      : /your/repo/.neurofs/index.db
  time       : 210ms

  Ready. Run: neurofs ask "<your question>" --budget 8000
```

### `neurofs ask <query>`

Ranks the index, selects context within the token budget, and prints an auditable bundle to stdout.

```
neurofs ask "how does auth work?" --budget 4000
neurofs ask "where are database migrations?" --format json
```

Stderr shows a ranking summary:
```
  [✓] src/auth/middleware.ts                score=8.50
  [✓] src/auth/jwt.ts                       score=6.00
  [ ] src/config/app.config.ts              score=1.30

  tokens used : 413 / 4000 (10.3%)
  files       : 2 included / 32 considered
  compression : 8.2x
```

Stdout receives the bundle (pipe to a file or copy directly).

### `neurofs pack <query>`

Same as `ask`, but writes the bundle to a file. Prefer `pack` when you are
about to paste the result into an LLM — add `--for claude` to get a
prompt-shaped output with grounding instructions.

```
neurofs pack "how does auth work?" --out auth.prompt --budget 6000
neurofs pack "database schema" --out schema.prompt --format json
neurofs pack "where is jwt verified" --for claude --out jwt.prompt
```

#### Flags that save real tokens

| Flag | What it does |
|------|--------------|
| `--for claude` | Prompt-shaped output, aggressive signature compression, grounding instructions appended. |
| `--focus <path[,path]>` | Strong additive boost to files under these prefixes. Use when you know which subtree matters. |
| `--changed` | Boost files in `git status`. No-op with a friendly message when the repo is not a git worktree. |
| `--no-chunks` | Disable chunk-based retrieval and pack whole files instead of relevant code chunks. |
| `--max-files N` | Cap on files included regardless of budget slack. |
| `--max-fragments N` | Cap on fragments included regardless of budget slack. |

### `neurofs mcp`

Runs a Model Context Protocol server over stdio (newline-delimited
JSON-RPC 2.0). Stdout carries protocol traffic; stderr carries logs.
The server exposes these tools to any MCP host:

- **`neurofs_context`** — broker entry point that routes a question to
  outline, search, excerpt, or a chunk-backed prompt bundle and returns a
  `tool_trace` plus `structural_hints` from indexed symbols/imports.
  Supports profile-like intents such as `research`, `review`, `test`, and
  `build` with different limits/budgets.
- **`neurofs_task`** — pack a Claude-ready prompt for a query.
  Args: `query` (required), `repo` (default: cwd), `budget` (default: 3000).
- **`neurofs_scan`** — index a repo and return a read-only summary
  (file count, total size, top extensions). Args: `repo` (default: cwd).
- **`neurofs_view_file`** — read one repository-confined file by relative path.
- **`neurofs_get_outline`** — return the indexed file outline.
- **`neurofs_list_signatures`** — return compact signatures for one file.
- **`neurofs_get_excerpt`** — return query-matching code excerpts for one file.
- **`neurofs_search`** — return ranked code chunks with line ranges,
  snippets, scores, reasons, content hashes, exact `rg` matches, semantic
  matches, and graph dependency/working-set bridges.

### `neurofs watch [path]`

Runs an initial scan and then keeps `.neurofs/index.db` synchronized with
file changes using `fsnotify`. It debounces bursts of writes, ignores the
same directories as `scan`, updates modified files incrementally, removes
deleted files from the index, and refreshes the dependency graph after
changes.

#### Wire it into Claude Desktop

Edit `~/Library/Application Support/Claude/claude_desktop_config.json`
(macOS) and add:

```json
{
  "mcpServers": {
    "neurofs": {
      "command": "/absolute/path/to/neurofs",
      "args": ["mcp"]
    }
  }
}
```

Restart Claude Desktop. The NeuroFS tools appear under the tools menu.

#### Wire it into Cursor / any stdio MCP client

Same shape, using the host's MCP server config:

```json
{
  "neurofs": {
    "command": "/absolute/path/to/neurofs",
    "args": ["mcp"]
  }
}
```

#### Smoke-test the server by hand

```
printf '%s\n%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  | neurofs mcp
```

You should see two JSON-RPC responses: one with `protocolVersion`
and `serverInfo`, one listing the available tools with their input schemas.

---

## Output Formats

| Format | Flag | Description |
|--------|------|-------------|
| Markdown | `--format markdown` | Default. Human-readable, great for chat interfaces |
| JSON | `--format json` | Machine-readable, useful for pipelines |
| Text | `--format text` | Plain text without Markdown syntax |
| Claude | `--format claude` or `--for claude` | Prompt-shaped: task, repo summary, XML-tagged context, grounding instructions |

---

## Using NeuroFS to reduce LLM tokens

The point of `pack --for claude` is to stop copy-pasting whole files into a
Claude chat. Pick a task, point at a subtree, get a small, well-labelled
bundle back.

Three recipes, all runnable against this repository.

### Recipe 1 — improve ranking in isolation

```
neurofs scan .
neurofs pack "improve the ranking and explain signals better" \
    --for claude --focus internal/ranking \
    --budget 3000 --max-files 5 \
    --out /tmp/ctx-ranking.prompt
```

Typical output on this repo: **5 files, ~880 tokens, ~12x compression** over
the raw sources — instead of pasting 4k+ tokens of Go files manually.

### Recipe 2 — review only what you changed

```
neurofs pack "review my current edits" \
    --for claude --changed --max-files 8 \
    --budget 3500 \
    --out /tmp/ctx-changed.prompt
```

`--changed` leans on `git status`. When you're mid-feature, this keeps the
bundle pinned to your working set instead of re-surfacing unrelated files
that happen to lexically match.

### Recipe 3 — navigate a new subsystem

```
neurofs pack "how does the audit package validate citations" \
    --for claude --focus internal/audit \
    --budget 2500 --max-files 4 \
    --out /tmp/ctx-audit.prompt
```

Combine `--focus` with a tight `--max-files` to force a summary-level read:
signatures first, dive into a specific file in a follow-up prompt.

### What you paste

The resulting file is structured as:

```
<task> ... </task>
<repo> ... languages, entry point ... </repo>
<selection> bundle: 5 files of 51, 880 tokens of 3000 (11.8x compression) </selection>
<context>
  <file path="..." rep="signature" tokens=... reasons="focus,filename_match"> ... </file>
  ...
</context>
<instructions>
  - cite claims as `path:line`
  - ask for expansion instead of guessing
  ...
</instructions>
```

Drop the whole file into a fresh Claude conversation and start asking
follow-up questions — you keep the grounding contract and the model keeps
the receipts.

---

## Use NeuroFS with Claude and replay the answer

`neurofs audit replay` closes the loop between the bundle you sent and the
answer you got back. It parses citations, flags drift, optionally scores
fact recall, and persists the verdict next to the repo so you can track
grounding over time. No network calls — the model reply is just a text
file you paste in.

### Step 1 — pack the bundle (and snapshot it)

```
neurofs pack "explain how the ranker handles stemming" \
    --for claude --focus internal/ranking \
    --budget 3000 --max-files 5 \
    --out /tmp/q.prompt \
    --save-bundle audit/bundles/stemming.json
```

`--save-bundle` writes the exact bundle JSON next to the repo. You'll use
it in step 3 so replay audits the bytes Claude actually saw, not a
re-packed approximation.

### Step 2 — paste into Claude, save the reply

Paste `/tmp/q.prompt` into a fresh Claude conversation. When the answer
comes back, save it to disk:

```
# select the reply and paste it into a file
pbpaste > /tmp/reply.md
```

### Step 3 — replay the audit

Two entry points:

```
# Replay against the saved bundle (recommended — audits the exact bytes)
neurofs audit replay --bundle audit/bundles/stemming.json \
    --response /tmp/reply.md --model claude-sonnet-4-6

# Or re-rank from the current index if you did not snapshot the bundle
neurofs audit replay "explain how the ranker handles stemming" \
    --response /tmp/reply.md --repo . --focus internal/ranking \
    --budget 3000 --max-files 5
```

Terminal output:

```
NeuroFS — audit replay
  bundle hash  : 3f7a9c4e...
  response     : /tmp/reply.md (1842 chars)
  citations    : 4 valid / 1 invalid
  grounded     : 80.0%
  drift rate   : 6.3%
  top drift    : legacyRanker, PhantomScorer
  record       : audit/records/1745260133-3f7a9c4e.json
```

### Step 4 — (optional) grade fact recall

Benchmark questions can carry `expects_facts` — short substrings a good
answer should mention. When replay sees them, it reports recall alongside
grounding:

```
neurofs audit replay --bundle audit/bundles/stemming.json \
    --response /tmp/reply.md \
    --facts "term variants,lowercase stem"
```

Pass `--facts-file path/to/facts.txt` (one fact per line) for longer
lists, or let the benchmark file supply them automatically when you pair
replay with `neurofs bench`.

### Step 5 — track grounding across runs

Every replay writes a JSON record under `audit/records/`. `neurofs stats`
aggregates whatever is there:

```
  audit records : 7 replayed
    grounded    : 86.4%
    drift       : 4.9%
    fact recall : 71.2%
    by model    : claude-sonnet-4-6=5, claude-manual=2
```

Commit the records if you want the history to travel with the repo; they
are plain JSON and small.

### Bench as a CI gate

`neurofs bench --bundle` now reports mean/p50/p95 bundle token counts.
`--search` adds the same style of report for `neurofs_search`: top-k
chunk hits, returned-token counts, p50/p95 latency, fact recall over
returned snippets, token ratio versus bundle output, and optional stable
JSON prefix checks. `--context` exercises the `neurofs_context` broker
itself, reporting route distribution, structural hint counts, output
tokens, latency, and top-k hits from routed results.
Combine the ceilings to fail the job when either retrieval quality drops
or context gets fatter:

```
neurofs bench --bundle --prefer-signatures --search --context --search-stability \
    --min-top3 75 \
    --max-mean-bundle-tokens 1200 \
    --max-mean-search-tokens 700 \
    --max-mean-context-tokens 900
```

If the ranker regresses *or* the packager starts emitting bigger bundles
for the same questions, the exit code is non-zero. With `--search` and
`--context`, the same gate also catches chunk retrieval and broker output
bloat.

---

## Context Representations

Each file can appear in the bundle in one of four forms, chosen automatically based on
relevance score, file size, and remaining budget:

| Representation | When used |
|---|---|
| `full_code` | File is small and fits the budget |
| `signature` | File is medium-sized; exports and symbols shown |
| `structural_note` | File is large or budget is tight; metadata only |
| `summary_placeholder` | Reserved for LLM-based summarisation (future) |

---

## Supported Languages

| Language | Extensions | Extracted |
|---|---|---|
| TypeScript | `.ts`, `.tsx`, `.mts` | imports, exports, functions, classes, types |
| JavaScript | `.js`, `.jsx`, `.mjs`, `.cjs` | imports, require, functions, classes |
| Python | `.py` | imports, functions, classes |
| Go | `.go` | imports, functions, types, consts |
| Rust | `.rs` | imports (use/mod), functions, structs, enums, traits, impls |
| C++ | `.cpp`, `.hpp`, `.cc`, `.h` | imports (include), class/struct, functions |
| Java | `.java` | imports, class/interface/enum, methods |
| Ruby | `.rb` | imports (require), class/module, methods (def) |
| Markdown | `.md`, `.mdx` | headings (h1–h3) |
| JSON/YAML | `.json`, `.yaml`, `.yml` | structural note |

---

## How Ranking Works

Scoring is hybrid, combining lexical, structural, and semantic signals. If an `OPENAI_API_KEY` is present, it uses `text-embedding-3-small` for semantic similarity; otherwise, it falls back to a deterministic local mock embedding generator to remain offline-safe.

| Signal | Weight | Description |
|---|---|---|
| `semantic_match` | +4.0 | Cosine similarity between query and file embeddings |
| `focus` | +4.0 | Boost for files explicitly targeted by user or inherited |
| `root_doc` | +3.5 | Floor boost for canonical repo root docs (README, CONTRIBUTING) |
| `filename_match` | +3.0 | Query term in the file name |
| `symbol_match` | +2.5 | Query term in a symbol name |
| `changed` | +2.0 | Boost for files with uncommitted git changes |
| `path_match` | +1.5 | Query term in the directory path |
| `entry_point` | +1.5 | Boost for entry point files |
| `dependency_match` | +1.2 | Boost for direct dependencies of high-scoring files |
| `import_match` | +1.0 | Query term in an import path |
| `import_expansion` | +0.8 | File imported by a high-scoring file |
| `content_match` | +0.5 | TF-style term frequency in file content |
| `lang_bonus` | +0.3 | Preference for code over config |

Every reason is included in the bundle — the ranking is fully auditable.

---

## Architecture

```
cmd/neurofs/          — entry point
internal/
  models/             — shared data types
  config/             — configuration and defaults
  fsutil/             — file system helpers, language detection
  storage/            — SQLite persistence (database/sql + modernc.org/sqlite)
  embeddings/         — OpenAI and mock vector embeddings for semantic ranking
  parser/             — symbol and import extraction (mostly regex)
  project/            — metadata extraction (package.json, tsconfig.json) to aid ranking
  indexer/            — orchestrates walk/watch → parse → store → graph
  ranking/            — lexical + structural relevance scoring
  packager/           — token-budget-aware bundle assembly
  tokenbudget/        — token estimation and budget management
  output/             — markdown / json / text serialisation
  taskflow/           — orchestrates the multi-step task pipeline
  audit/              — citation/drift/fact scoring + replay persistence
  quality/            — records human ratings of task prompts for feedback loops
  gate/               — pivot-readiness criteria evaluation (G1–G5)
  benchmark/          — curated (question → expected-file) ranking bench
  ui/                 — local web UI server, API handlers, and AI proxy
  mcp/                — Model Context Protocol (MCP) server implementation
  cli/                — cobra commands: scan, ask, pack, stats, bench, audit, gate
testdata/
  sample-repo/        — realistic sample repository for tests
```

---

## Development

```bash
make deps         # go mod tidy
make build        # compile binary to ./bin/neurofs
make install      # install system-wide to $GOPATH/bin
make test         # run all tests including integration
make test-short   # run tests skipping integration tests
make run-ui       # start the local web UI
make run-scan     # scan testdata/sample-repo
make run-ask      # ask against testdata/sample-repo
make vet          # go vet
make fmt          # gofmt
make lint         # run golangci-lint
make help         # print all available targets
```

---

## Roadmap

**Phase 1 — Package** *(shipped)*  
Local context packager. Scan, rank, compress, export. No external
dependencies. `scan` / `ask` / `pack` cover the bare metal.

**Phase 2 — Govern** *(shipped)*  
`audit replay` parses citations and grounds the model's answer against
the bundle. The local UI exposes the journal, compare, global search,
human metadata (title/brief/note) per run, mode presets
(strategy/build/review), and the resume flow — pick a previous run,
inherit its focus paths, continue with a parent_record breadcrumb on
the new audit so the causal chain survives on disk. EN/ES i18n.

**Phase 3 — One-shot** *(shipped)*  
`neurofs task <query>` collapses scan → rank → pack → prompt into a
single command, with a `(query, budget)` cache. The shared
`internal/taskflow` package keeps CLI and UI behaviour identical.

**Phase 4 — MCP** *(shipped)*  
`neurofs mcp` runs a Model Context Protocol server over stdio,
exposing task, scan, outline, signatures, excerpts, and safe file-view
tools, plus chunk-level search and the `neurofs_context` broker any MCP
host (Claude Desktop, Cursor, etc.) can call. The broker uses indexed
symbols/imports to promote symbol-shaped questions to excerpts and to
boost routed search results. JSON-RPC 2.0, no extra dependencies,
stdout reserved for protocol traffic.

**Phase 5 — Living index** *(in progress)*
`neurofs watch` keeps the SQLite index fresh with `fsnotify`, while the
packager already has a Go AST excerpt path for precise sub-file context.
The index now persists chunks with content hashes and exposes an MCP
search tool over those chunks, including semantic scoring from cached
chunk embeddings, graph expansion through direct imports, and git
working-set boosts. Exact identifier and filename matches now add
`exact_content` / `exact_filename` reasons, while very large chunks lose
rank when smaller alternatives match. `neurofs pack` (or `neurofs task`) now uses
chunk-based retrieval by default to build bundles from those chunk hits (opt out with
`--no-chunks`), carrying the same line-ranged retrieval into the one-shot prompt flow.
Next: AST-backed chunking for TS/JS/Python.

**Strategic planning**
Current competitive radar and implementation direction live in
[`docs/strategic_anticipations.md`](docs/strategic_anticipations.md) and
[`docs/implementation_plan.md`](docs/implementation_plan.md).

**Next**  
Hybrid agentic search, MCP broker routing, session ledger, progressive
expansion, and attention routing for large context windows.

---

## Principles

- **Selection over accumulation** — more context is not automatically better.
- **Structure over flat text** — relationships matter, not just keywords.
- **Compression with caution** — summaries can erase critical invariants.
- **Traceability by default** — every inclusion has a reason.
- **Model-agnostic** — the output works with any LLM, any interface.

---

## License

MIT

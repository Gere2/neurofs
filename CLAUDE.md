# NeuroFS

Go CLI + MCP server that packs minimal, citable code context for LLM agents.
Build: `go build ./...` · Test: `go test ./...` · Binary the MCP registration
launches: `go build -o bin/neurofs ./cmd/neurofs` (rebuild after MCP-visible
changes or the running server keeps the old behavior).

## Retrieval (dogfooding the learn loop)

- Before reading whole files, ask NeuroFS first: use `neurofs_context` (or
  `neurofs_search`) to get targeted, citable excerpts.
- After finishing a task that used those results, call `neurofs_feedback`
  once: rating `yes`/`no`/`partial`, the symbols/paths that actually helped,
  and any identifier that should have been retrieved but wasn't. Only name
  symbols you verified exist. This feeds `neurofs learn` (see
  docs/self_improvement.md).

## Guardrails

- `neurofs gate` is the pivot-readiness oracle (docs/PIVOT_GATE.md); never
  weaken a criterion to make it pass.
- Never apply a single-corpus tune: `neurofs learn tune` must include
  `--corpus <repo>:<fixtures-dir>` pairs (e.g. the G5 repos) before
  `--apply` — a single-repo tune measurably overfits (see
  docs/self_improvement.md). After applying, run `neurofs gate` and
  `neurofs bench --search` to confirm no regression.
- Ledgers under `.neurofs/` and `audit/` are append-only history — correct
  them with new entries, never by rewriting.

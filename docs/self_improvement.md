# Improve-through-use: the learn loop

NeuroFS improves its ranking from real usage. There is no model being
retrained — the "training" is a measured loop over the retrieval scoring
weights, fed by two append-only ledgers that fill themselves while you work.

```
 use it            judge it              grow the oracle        optimize
 ────────►  MCP    ────────►  agent      ─────────────►  learn  ────────►  learn tune
 neurofs_search /  logs to    calls      promote joins   writes           coordinate descent
 neurofs_context   usage.jsonl neurofs_  feedback+usage  audit/facts/     over weights, recall
 (every call       (query,    feedback   into fixtures   learned-*.json   first, tokens second
  logged free)      hits,     (useful/                                    --apply persists to
                    tokens)    missing)                                    .neurofs/weights.json
                                                        └──── gate G3 reads the same fixtures ────┘
```

## The pieces

| piece | file | written by |
|---|---|---|
| usage ledger | `.neurofs/usage.jsonl` | every MCP `neurofs_search` / `neurofs_context` call |
| feedback ledger | `.neurofs/feedback.jsonl` | the `neurofs_feedback` MCP tool |
| learned fixtures | `audit/facts/learned-*.json` | `neurofs learn promote` |
| tuned weights | `.neurofs/weights.json` | `neurofs learn tune --apply` |

- **Usage** records what retrieval delivered: query, ranked hits with their
  reasons, token estimate. Logging is best-effort and never fails a search.
- **Feedback** is the post-task judgement: rating (`yes`/`no`/`partial`),
  which paths/symbols actually mattered, which identifiers were *missing*.
  A `no` with `missing` facts is the most valuable entry — it becomes the
  strictest fixture.
- **Promote** turns each distinct judged query into a G3-style fixture
  (identifier-shaped facts, 6 max). Later feedback on the same query
  replaces earlier judgements at promote time; existing fixture files are
  never overwritten, so hand-tweaks survive. Because learned fixtures live
  in `audit/facts/`, the pivot gate's G3 evaluates them automatically.
- **Tune** runs coordinate descent over the scoring weights in
  `internal/retrieval/weights.go`, evaluating every fixture on the search
  surface with `audit.ScoreFacts` — the identical scorer G3 and the Phase-0
  economy harness use. Objective: mean recall first, fewer delivered tokens
  at equal recall. Dry-run by default; `--apply` writes
  `.neurofs/weights.json`, which every subsequent search loads (MCP, CLI,
  bench, gate — the tuned engine is what gets gated, not a fork of it).

## Daily workflow

Adopting a new repo is one command — it indexes, wires the CLAUDE.md
retrieval contract, and prints the one-time MCP registration:

```
cd /path/to/project && neurofs setup
```

After that there is nothing to do beyond using the tool: retrievals log
themselves and the agent reports feedback per the CLAUDE.md contract.
Then, occasionally:

```
neurofs learn status          # how much signal has accumulated
neurofs learn promote         # feedback -> fixtures
neurofs learn tune            # dry run: what would change, how much it helps
neurofs learn tune --apply    # adopt the winning weights
neurofs gate && neurofs bench --search   # independent guardrails
```

For any tune you intend to apply, add cross-shape corpora so the objective
is the macro-average across repository shapes instead of one repo's fixtures:

```
git clone --depth 1 https://github.com/pallets/click /tmp/click && neurofs scan /tmp/click
neurofs learn tune --corpus /tmp/click:docs/g5_fixtures/click
```

This is not optional hygiene — it is the measured failure mode. The first
single-repo tune (8 fixtures) lifted in-sample recall 84.4% → 93.8% while
dropping click from 67% to 40% recall with a new search miss. The
macro-average objective prevents exactly that trade.

## First applied tune (2026-07-04, evidence)

The first multi-corpus tune (8 NeuroFS + 5 click fixtures, ~2.5 min with
session-cached search) was adopted after clearing every guardrail on the
same working tree:

| metric (same tree, defaults → joint-tuned) | defaults | tuned |
|---|---|---|
| NeuroFS economy recall / token reduction | 75% / 49.0% | **84% / 70.6%** |
| click economy recall / token reduction | 67% / 83.0% | 67% / 82.9% |
| gate G3 (bundle surface) | 91%, 6 perfect | 91%, 6 perfect |
| G2 / G4 | PASS | PASS |

Notable: the joint objective pushed `symbol_exact` UP (6 → 11.2) where the
overfit single-repo tune had pushed it DOWN (6 → 3.6) — click's recall
depends on it. Macro-averaging across shapes is what flipped the direction.
Economy baselines drift with the working tree (the working-set boost sees
uncommitted files), so always compare defaults vs tuned on the same tree,
never against numbers from another day.

## Second applied tune (2026-07-04, after markdown section chunking)

Markdown files used to index as one whole-file chunk (2.3k tokens average);
heading-level section chunks fixed a doc-retrieval gap found through real
use, but sections competing with code nudged one Go fixture
(`retrieval-ripgrep`) off the top-8 — a real trade the fixture set caught.
Instead of hand-tweaking, the answer was a fresh 3-corpus tune over all 20
fixtures (9 NeuroFS + 5 click + 6 vue):

| corpus | before | after |
|---|---|---|
| NeuroFS (search recall) | 80.6% | **91.7%** |
| click | 66.7% | 66.7% (tokens −4%) |
| vue | 50.0% | 50.0% (tokens −8%) |

Applied after guardrails: economy **92% recall / 0 miss / 70.0% reduction**
(best this repo has measured), G3 97% (8 perfect), G2/G4 PASS, click PASS
84.6%, vue WARN unchanged. Main movements: `symbol_match` 10 → 18.8 while
`content_match` 0.72 and `exact_content` 2.7 dropped — with sections and
nested closures indexed, symbol identity got more reliable than raw content
hits, and the tuner noticed before anyone hand-reasoned it.

CLAUDE.md snippet for any repo where NeuroFS is registered:

```markdown
## Retrieval
- Before reading whole files, ask NeuroFS: use `neurofs_context` (or
  `neurofs_search`) to get targeted, citable excerpts.
- After finishing a task that used those results, call `neurofs_feedback`
  once: rating yes/no/partial, the symbols/paths that actually helped, and
  any identifier that should have been retrieved but wasn't.
```

## Honesty constraints (read before trusting a tune)

1. **Overfit warning.** Under 10 fixtures the tuner will happily overfit;
   the report says so explicitly. Accumulate real feedback before trusting
   an applied tune, and prefer cross-shape checks (G5 repos) for any large
   weight movement.
2. **The tuner optimizes the search surface.** The gate's G3 scores the
   bundle surface; `bench` scores ranker precision on a synthetic corpus.
   Those stay independent — a tune must survive them, not replace them.
3. **Ledgers are append-only.** A wrong feedback entry is corrected by a
   newer entry for the same query, never by editing history.
4. **Weights are per-repo.** `.neurofs/weights.json` travels with the repo
   index, not with the binary; a repo without one uses the hand-calibrated
   defaults in `DefaultWeights()`.

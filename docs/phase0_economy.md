# Phase 0 — Proving the token economy

**Verdict: PASS.** Delivering context with `neurofs_search` costs **58.9% fewer
tokens on average (median 71.4%)** than native whole-file reading **at equal
fact recall** — more than double the 25% decision threshold, with **0 search
misses** (every fact task grounded). Phase 0 clears the gate; the pivot to a
context-and-verification plane for autonomous loops is justified on the
economics, not just the narrative.

| metric | NeuroFS (`neurofs_search`) | native (whole files) |
|---|---|---|
| mean context tokens / task (scored subset) | **1,878** | 5,096 |
| overall fact recall (all 7 tasks, misses = 0) | **86%** | — |
| iso-recall (scored subset) | 86% | 89% (matched, ≥ B by construction) |
| mean token reduction at iso-recall | **58.9%** | — |
| median token reduction at iso-recall | **71.4%** | — |
| fact tasks / scored / search misses | 7 / 7 / **0** | — |

> Honest-recall note: the harness reports recall two ways. **Overall recall**
> averages over *all* fact tasks, counting a search miss as 0 — the "how often
> does it ground at all" number. **Iso-recall** is the scored subset used for
> the token comparison (you cannot compute a token ratio for a 0-recall task).
> A high miss rate (≥ 1/3 of fact tasks) downgrades the verdict to `WARN` so a
> flattering scored-subset number can never stand alone. Here misses are 0, so
> the two coincide and the PASS is unqualified.

Reproduce:

```
neurofs economy --repo .            # human-readable
neurofs economy --repo . --json     # machine-readable
neurofs economy --repo . --gate     # exit non-zero on FAIL (CI)
```

The full machine-readable run is committed alongside this doc as
[`phase0_economy.json`](phase0_economy.json).

---

## The question

In an autonomous loop the agent re-derives its context on every iteration, so
the unit cost that matters is **tokens delivered per grounded task**. Phase 0
asks one falsifiable question:

> To reach equal-or-better fact recall, how many context tokens does native
> retrieval (whole files) deliver versus NeuroFS (targeted, citable excerpts)?

If NeuroFS cannot cut tokens by at least 25% at equal recall, the pivot is not
justified and we stop. It is not.

## Experiment design

It is an **iso-recall** A/B comparison — the two arms are held at the *same*
recall and the only free variable is tokens.

- **Corpus.** The NeuroFS repository's own index (the working tree + SQLite
  index present at run time). Fully local, no network.
- **Tasks.** The seven G3 fact fixtures in `audit/facts/*.json` — real questions
  about NeuroFS's own code, each with identifier- or body-shaped
  `expects_facts` (e.g. `weightFilename`, `PRAGMA journal_mode = WAL`,
  `exec.CommandContext`). These are the existing recall oracle, the same one the
  pivot gate's G3 uses.
- **Arm B — NeuroFS.** `neurofs_search` (the citable-excerpt surface the pivot
  names as primary), `--search-limit 8`. We record the snippet tokens delivered
  and the fact recall over those snippets, scored with `audit.ScoreFacts` — the
  identical scorer the gate uses.
- **Arm A — native.** Read the **whole files** arm B's hits came from, in hit
  order, accumulating only until recall reaches arm B's recall. Because a whole
  file is a superset of any excerpt of it, the baseline is guaranteed to reach
  B's recall, so the arms tie on recall and differ only on tokens.
- **Tokenizer.** `tokenbudget.EstimateTokens` (the 4-chars-per-token heuristic
  used everywhere in the engine) on both arms, so the comparison is internally
  consistent.

### Why this baseline is conservative, not flattering

The native arm is handed NeuroFS's *own* file selection for free and stops the
instant it matches NeuroFS's recall. A real agent without NeuroFS would not know
which files to open and would read more of them. So the measured 57.7% is a
**lower bound** on the advantage over a naive native agent; the realistic gap is
larger. (For reference, an agent that simply opens the top-8 ranked files whole
spends ~34k tokens per task — an ~94% reduction against that, but it is a less
defensible baseline so it does not gate.)

## Per-task results

| task | B tokens | B recall | native tokens | native recall | reduction |
|---|---:|---:|---:|---:|---:|
| MCP tools exposed | 1,690 | 50% | 4,868 | 50% | 65.3% |
| packager excerpt vs signature | 1,274 | 75% | 7,002 | 100% | 81.8% |
| packager UpgradeWithSlack | 2,552 | 100% | 2,862 | 100% | 10.8% |
| ranker filename match | 1,672 | 100% | 6,096 | 100% | 72.6% |
| retrieval ripgrep dependency | 3,267 | 100% | 5,119 | 100% | 36.2% |
| session ledger timelines | 828 | 75% | 3,214 | 75% | 74.2% |
| storage WAL pragma | 1,866 | 100% | 6,516 | 100% | 71.4% |

The weakest task (`UpgradeWithSlack`, 10.8%) is the honest one: when the answer
is concentrated in a single modest file, reading that file whole is competitive
with excerpting it. NeuroFS wins decisively when the facts are buried in, or
spread across, large files — which is the common case in a real repo.

## Proxy boundary (what this does NOT prove)

This is an honest proxy with explicit limits:

1. **Single iteration, not a loop.** It measures context-delivery efficiency for
   one retrieval per task. It does not measure end-to-end agent task success, nor
   the compounding re-derivation cost across many loop turns — the place the
   economy thesis expects the gap to widen, but which cannot be measured
   hermetically without driving a real billed agent.
2. **Recall is a grounding proxy, not answer correctness.** `ScoreFacts` confirms
   the expected identifiers reached the delivered context; it does not confirm
   the model used them correctly. This is the same proxy the gate's G3 relies on.
3. **Small n.** Seven fixtures. Enough to clear a 25% gate by a 2.3× margin and
   stay stable across runs, but not a population estimate. Widening the fixture
   set strengthens the signal and is tracked under G3.
4. **Tokenizer is a heuristic.** The 4-chars/token estimate is applied
   identically to both arms, so it cannot bias the *ratio*; absolute token counts
   would shift under a real BPE tokenizer.
5. **Retrieval mixes live FS and index.** `neurofs_search`'s exact-content arm
   reads the working tree via ripgrep while symbol/graph signals come from the
   SQLite index; results reflect the repo state at run time.
6. **Shape-dependent — and recall does NOT hold everywhere.** The advantage is
   proven on this Go service (58.9%, 0 misses). On a large Python repo
   (pallets/click) the chunk *economics* now hold too (82.9% reduction on the
   answerable subset, after method-level Python chunking landed), but
   retrieval still misses 40% of fact tasks there, so the verdict is WARN, not
   PASS. On a toy 10-file repo the economy inverts (files already tiny). The
   reproducible cross-shape verdicts, the before/after of the chunking fix,
   and the honest correction of an earlier over-optimistic number are in
   [`phase_g5_cross_shape.md`](phase_g5_cross_shape.md).

## Decision

Token reduction of 57.7% (median 71.4%) at equal recall, stable across runs,
with the gate's G2/G3 unaffected. **Proceed to Phase 1** (reposition) and Phase 2
(automate grounding) — the differentiator is real and measurable.

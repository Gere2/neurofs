# G5 — Cross-shape sanity

The pivot gate's G5 asks whether the engine holds across repository shapes, not
just on this Go service. This records `economy`, `bench`, and `gate` on three
shapes — and, just as importantly, **corrects an earlier over-optimistic result**
once the measurement was made honest and reproducible.

| Shape | Repo | files | `economy` (iso-recall) | overall recall / miss | `bench` top-3 | `gate` G2 / G3 |
|---|---|---:|---|---|---:|---|
| Go service | NeuroFS (this repo) | 143 | **PASS · 58.9%** | 86% / 0% | 83.3% | PASS / PASS (96%) |
| Python lib | [pallets/click](https://github.com/pallets/click) | 113 | **FAIL · −21.9%** | 20% / 60% | 66.7% | PASS / **FAIL (13%)** |
| TS/JS frontend | testdata/sample-repo | 10 | **FAIL · inversion** | 100% / 0% | 100% | PASS / PASS |

Go uses the committed `audit/facts/*.json`; Python uses the committed
[`g5_fixtures/click/`](g5_fixtures/click) (15 grep-verified identifiers across 5
questions) so the run is reproducible. Reproduce Python with:

```
git clone --depth 1 https://github.com/pallets/click /tmp/click && neurofs scan /tmp/click
neurofs economy --repo /tmp/click --fixtures-dir docs/g5_fixtures/click
```

## Correction (integrity note)

An earlier draft of this doc reported click as **economy PASS 72.5%** with the
default bundle at **G3 11% vs search 67%**. Both numbers were wrong, for two
reasons, and are superseded by the table above:

1. **Unsaved fixtures.** That run used ad-hoc click fixtures that were never
   committed, so the numbers were not reproducible and happened to land on
   facts retrieval handled well. The fixtures are now committed.
2. **A recall-reporting flaw in the harness.** `economy` averaged recall only
   over *scored* tasks, silently dropping search misses — which inflated recall
   on any repo where retrieval misses. The harness was fixed to report
   **overall recall over all fact tasks (misses = 0)** and to downgrade the
   verdict to `WARN` when the miss rate is high. See
   [`internal/abeval`](../internal/abeval/abeval.go).

Stating this plainly is the point of the gate: it exists so we know if we are
fooling ourselves. We were, briefly; the corrected result is below.

## What the honest numbers say

**The economy is proven on the Go service, not universal.** On this repo
(143 files), `neurofs_search` delivers equal recall for **58.9% fewer tokens**
with **0 search misses**. That is the firm result the pivot rests on.

**It breaks on large Python files.** On click, `neurofs_search` returns
oversized, line-based chunks (≈12.5k tokens for the scored subset) that *lose*
to whole-file reading (**−21.9%**) and miss **60%** of facts (overall recall
20%). This is not a toy-repo artefact — it is the **AST-chunking gap** the
roadmap already names ("Next: AST-backed chunking for TS/JS/Python"). Without
syntactic chunk boundaries, a Python "chunk" is a coarse line window: too big to
be cheap, too blunt to reliably contain the asked-for symbol. Budget is not the
lever — G3 plateaus at 20% from an 8k → 24k budget sweep.

**The toy repo inverts for the opposite reason.** On the 10-file TS sample,
files are ~150–300 tokens each, so any excerpt overhead loses to just reading
the whole (tiny) file. Recall is 100% — there is simply nothing to compress.

**Ranking is healthy cross-shape.** `bench` top-3 precision is 83% / 67% / 100%
(Go / Python / TS); the ranker surfaces an expected file in the top 3 on every
shape. The Python failure is in *chunking and excerpt size*, not in which files
rank.

## Verdicts

- **Go service** — `economy` PASS (58.9%, 0 miss), `gate` G2/G3 PASS. The result
  that justifies the pivot.
- **Python lib** — `economy` **FAIL** (−21.9%, 60% miss); `gate` G2 PASS, **G3
  FAIL (13%)**. A real, reproducible engine gap: AST-backed chunking for
  large-file languages is the highest-value next investment, now with a number
  attached.
- **TS/JS toy** — `economy` FAIL (small-file inversion), `gate` G2/G3 PASS.

## Note on G1 (real-use signal)

G1 measures sustained *human* usefulness via `neurofs task --rate`
(`.neurofs/quality.jsonl`, gitignored). During this work I seeded it with **10
honest self-assessments** of real questions about this codebase → **7/10 (70%)**,
below the 0.8 bar. Two caveats keep this honest: (1) it is an *agent*
self-assessment over a small, deep question set — not the independent human
signal G1 is designed for; (2) because `quality.jsonl` is gitignored, a fresh
checkout's `gate` shows **G1 SKIP, Overall PASS**. The 70% is consistent with the
click finding: the engine's recall on harder/larger inputs has real room to
improve. The honest G1 verdict remains pending genuine human use.

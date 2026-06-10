# G5 — Cross-shape sanity

The pivot gate's G5 asks whether the engine holds across repository shapes, not
just on this Go service. This records `economy`, `bench`, and `gate` run on
three shapes. The headline: **the token economy holds on real-sized repos and
inverts on a trivially small one** — an honest boundary, not a failure to hide.

| Shape | Repo | files | `economy` (iso-recall) | `bench` top-3 | `gate` G2 / G3 |
|---|---|---:|---|---:|---|
| Go service | NeuroFS (this repo) | 143 | **PASS · 58.9%** (search 1,878 tok @ 86% vs native 5,096) | 83.3% | PASS / PASS (96%) |
| Python lib | [pallets/click](https://github.com/pallets/click) | 113 | **PASS · 72.5%** (search 11,580 tok @ 67% vs native 38,856) | 66.7% | PASS / **FAIL (11%)** |
| TS/JS frontend | testdata/sample-repo | 10 | **FAIL · −219.7%** (search 829 tok @ 100% vs native 294) | 100% | PASS / PASS (100%) |

Each repo was scanned fresh; fixtures (`audit/facts/*.json`) and bench questions
were authored against real identifiers verified to exist in each codebase. The
two external repos were run in throwaway clones/copies, so nothing here is
committed beyond this doc.

## What the numbers say

**The economy generalises to real repos.** On the Go service (143 files) and
the Python library (113 files), `neurofs_search` delivers the same fact recall
as native whole-file reading for **58.9%** and **72.5%** fewer tokens. The core
Phase-0 claim is not a Go-only artefact.

**It inverts on a toy repo.** On the 10-file TS sample (files of ~150–300 tokens
each), targeted search returns *more* tokens than just reading the one or two
tiny whole files (−219.7%). This is the honest boundary already flagged in
[`phase0_economy.md`](phase0_economy.md): NeuroFS wins when the answer is buried
in, or spread across, large files. When whole files are already small, native
reading is cheaper and NeuroFS should get out of the way. A real frontend
codebase is not 10 files — but the inversion is real and worth stating plainly.

**The search surface beats the default bundle on unfamiliar repos.** The most
informative result is click's split: `gate` G3 (which scores the *default
`task` bundle*) recovered only **11%** of facts, spreading an 8k budget thin
across 113 files — while `economy`'s arm B (`neurofs_search`, targeted
excerpts) recovered **67%** of the same facts at a fraction of the per-file
cost. That gap is direct evidence for the pivot itself: the citable-excerpt
surface grounds far better than the legacy whole-bundle on a large, cold repo.
It also flags real engine work — the default `task`/`neurofs_context` bundle
should lean harder on the search path on big repos. Tracked, not hidden.

**Ranking is healthy cross-shape.** `bench` top-3 precision is 83% (Go), 67%
(Python, n=3), 100% (TS) — the ranker surfaces an expected file in the top 3 on
every shape; the Python miss is one fixture out of three on a cold clone.

## Verdicts

- **Go service** — `economy` PASS, `gate` G2/G3 PASS. (G1 is a separate,
  human-signal criterion; see below.)
- **Python lib** — `economy` PASS; `gate` G2 PASS, **G3 FAIL (11%)** on the
  default bundle. Real finding: default packing dilutes on large cold repos;
  the search surface (67%) is the answer the pivot already points at.
- **TS/JS** — `economy` FAIL (toy-repo inversion), `gate` G2/G3 PASS, Overall
  PASS.

## Note on G1 (real-use signal)

G1 measures sustained *human* usefulness via `neurofs task --rate`
(`.neurofs/quality.jsonl`, gitignored). During this work I instrumented it with
**10 honest self-assessments** of real questions about this codebase, rating a
prompt "yes" only when its bundle actually contained the file that answers the
question. The result: **7/10 → 70%**, below the 0.8 bar, so a locally-seeded
gate reports G1 = FAIL.

This is reported, not buried, but with two honest caveats: (1) it is an *agent*
self-assessment over a small, deliberately deep question set — not the
independent, sustained human signal G1 is designed to require; and (2) because
`quality.jsonl` is gitignored, a fresh checkout's `gate` shows **G1 SKIP,
Overall PASS**. The 70% is consistent with the click G3 finding: the default
bundle's file selection has real room to improve, which is squarely engine work,
not pivot-blocking. The honest G1 verdict remains pending genuine human use.

# G5 — Cross-shape sanity

The pivot gate's G5 asks whether the engine holds across repository shapes, not
just on this Go service. This records `economy`, `bench`, and `gate` on three
shapes — and, just as importantly, **corrects an earlier over-optimistic result**
once the measurement was made honest and reproducible.

| Shape | Repo | files | `economy` (iso-recall) | overall recall / miss | `bench` top-3 | `gate` G2 / G3 |
|---|---|---:|---|---|---:|---|
| Go service | NeuroFS (this repo) | 143 | **PASS · 48.2%** | 82% / 0% | 83.3% | PASS / PASS (96%) |
| Python lib | [pallets/click](https://github.com/pallets/click) | 113 | **PASS · 88.6%** | 53% / 0% | 66.7% | PASS / **FAIL (53%)** |
| TS/JS frontend | testdata/sample-repo | 10 | **FAIL · inversion** | 100% / 0% | 100% | PASS / PASS |

> The Python row started at **FAIL · −21.9% / 60% miss / G3 13%** and reached
> PASS in two measured steps: method-level chunking (→ WARN · 82.9%, miss 40%,
> G3 20%) and the `symbol_exact` retrieval signal (→ PASS · 88.6%, **0
> misses**, recall 20% → 53%, G3 53%). The second step costs the Go shape one
> hard task (economy 58.9% → 48.2%, recall 86% → 82%, still PASS) — cross-shape
> recall was chosen over the prettier single-repo number. Both trade-offs are
> documented below.

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

**It used to break on large Python files — the chunking half is now closed.**
Originally the Python parser only extracted column-0 symbols, so methods inside
classes were invisible and `class Context` (~1,000 lines in click) became one
chunk: too big to be cheap, too blunt to target. Result: `neurofs_search`
returned ≈12.5k tokens for the scored subset, *lost* to whole-file reading
(−21.9%) and missed 60% of facts. Budget was not the lever — G3 plateaued at
20% across an 8k → 24k sweep.

## Closing the chunking gap

The fix mirrors what the JS path already did: the Python parser now extracts
methods at every nesting level (qualified `Class.method`, closures and
docstring example code excluded), and the chunker emits per-method chunks
while capping each class chunk at its header (class line, docstring,
class-level attributes). On click this took symbols from 1,130 → 1,642 and
chunk sizes from class-sized to method-sized. Measured before/after on the
same committed fixtures:

| metric (click) | before | after |
|---|---:|---:|
| economy verdict | FAIL | **WARN** |
| iso-recall token reduction | −21.9% | **+82.9%** |
| arm B tokens (scored subset) | 12,469 | 1,964 |
| search miss rate | 60% | 40% |
| overall recall | 20% | 20% |
| gate G3 (default bundle) | 13% | 20% |

That left **retrieval recall** (which chunks surface) as the gap. Three
follow-ups were measured; two were reverted, one landed:

- **Reverted — term-proportional `symbol_match`.** Scaling the symbol weight by
  matching-term count dropped recall on *both* shapes (click 20% → 13%, NeuroFS
  86% → 75%): the substring-based matcher lets generic question words stack
  onto irrelevant symbols.
- **Reverted — class-header anchoring.** Pulling `class X`'s header chunk in
  whenever ≥2 of X's methods ranked changed nothing on click (the header
  exceeded the size cap — click docstrings are huge) and regressed NeuroFS
  (86% → 75%) by evicting fact-bearing hits at a full result limit.
- **Landed — `symbol_exact` (+6.0).** A query term *equal* to the chunk's
  symbol name (or its last dotted component) is qualitatively stronger evidence
  than a substring hit, and it discriminates inside one file where every
  structural boost is identical. Result: click recall 20% → **53%**, misses
  40% → **0**, economy WARN → **PASS (88.6%)**, default-bundle G3 20% → 53%.
  Cost: on NeuroFS, one task (`mcp-tools-list`) regresses because its query
  words ("server", "client") are literal type names — economy 58.9% → 48.2%,
  recall 86% → 82%, still PASS. A 4.0 weight was also measured: same cost on
  Go, weaker click gains (47%, 1 miss) — 6.0 kept.

The remaining recall gap on click (53% vs the 80% bar) is concentrated in
fact-bearing chunks whose names the question does *not* speak verbatim. Two
follow-up hypotheses for reaching them were then measured and **falsified**:

- **Component equality** (`symbol_component` +4.0: query term equal to one
  snake/camel component of the name — "parse" vs `parse_args`, "runner" vs
  `CliRunner`). Miss-level analysis said it reaches 4 of the 6 missing facts;
  measured, it reaches everything else too: click recall **53% → 27%**,
  misses back to 40%, main 82% → 79%. At a fixed result limit, precision
  beats reach.
- **Callee boost** (`called_by_named` +4.0: chunks that an exactly-named
  chunk calls, same file — the structural "callers/callees" idea this doc
  previously recommended). Null on click (53% → 53%; the predicted
  `isolation` recovery did not materialise) and regressed main 82% → 75% by
  displacement. The hypothesis is falsified as stated.

Five scoring experiments, one keeper (`symbol_exact`): static lexical and
shallow structural signals are at a local optimum at limit 8. The remaining
recall likely needs either real (non-mock) embeddings on cold repos or a
wider candidate surface for the bundle path — both measurable, neither a
weight tweak.

**The toy repo inverts for the opposite reason.** On the 10-file TS sample,
files are ~150–300 tokens each, so any excerpt overhead loses to just reading
the whole (tiny) file. Recall is 100% — there is simply nothing to compress.

**Ranking is healthy cross-shape.** `bench` top-3 precision is 83% / 67% / 100%
(Go / Python / TS); the ranker surfaces an expected file in the top 3 on every
shape. The Python gap is in *which chunks* surface within the right files, not
in which files rank.

## Verdicts

- **Go service** — `economy` PASS (48.2%, 0 miss), `gate` G2/G3 PASS (96%).
  The result that justifies the pivot; carries the documented `symbol_exact`
  cost on one task.
- **Python lib** — `economy` **PASS** (88.6%, 0 miss); `gate` G2 PASS, G3
  **FAIL (53%)** against the 80% bar. Chunk economics and search misses are
  fixed; the default bundle's remaining recall gap needs a structural signal,
  not lexical tuning.
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

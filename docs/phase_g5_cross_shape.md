# G5 — Cross-shape sanity

The pivot gate's G5 asks whether the engine holds across repository shapes, not
just on this Go service. This records `economy`, `bench`, and `gate` on three
shapes — and, just as importantly, **corrects an earlier over-optimistic result**
once the measurement was made honest and reproducible.

| Shape | Repo | files | `economy` (iso-recall) | overall recall / miss | `bench` top-3 | `gate` G2 / G3 |
|---|---|---:|---|---|---:|---|
| Go service | NeuroFS (this repo) | 143 | **PASS · 42.1%** | 79% / 0% | 83.3% | PASS / PASS (93%) |
| Python lib | [pallets/click](https://github.com/pallets/click) | 113 | **PASS · 82.3%** | 67% / 0% | 66.7% | PASS / **FAIL (67%)** |
| TS/JS toy | testdata/sample-repo | 10 | **FAIL · inversion** | 100% / 0% | 100% | PASS / PASS |
| TS frontend | [vuejs/core](https://github.com/vuejs/core) | 599 | **PASS · 77.0%** | 61% / 17% miss | — | see below |

> The Python row started at **FAIL · −21.9% / 60% miss / G3 13%** and improved
> in three measured steps: method-level chunking (→ WARN · 82.9%, miss 40%,
> G3 20%), the `symbol_exact` retrieval signal (→ PASS · 88.6%, **0 misses**,
> recall 20% → 53%, G3 53%), and same-symbol dedupe plus a wider bundle
> candidate surface (→ recall **67%** on both surfaces, G3 **67%**). Each step
> costs the Go shape a few points (economy 58.9% → 42.1%, recall 86% → 79%,
> G3 96% → 93% — all still PASS); cross-shape recall was chosen over the
> prettier single-repo number. Every trade-off is documented below.

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

A fourth measured step then landed: **same-symbol dedupe + wider bundle
candidate surface**. Diagnosis showed click's per-file diversity cap (3
chunks/file) being filled by three `@t.overload` stubs of the *same* symbol
(`command` at decorators.py:138/144/153), squeezing `def option` and
`pass_context` out entirely; and the bundle path's search limit (12) cutting
candidates the 8k budget had room for. `dedupeSameSymbol` keeps one hit per
(path, symbol) — the implementation body, not a stub — and taskflow now
searches 24 candidates, letting the budget do the trimming. Result: click
recall 53% → **67%** on both the search and bundle surfaces; cost on Go:
economy 48.2% → 42.1%, G3 96% → 93% (both still PASS).

The remaining recall gap on click (67% vs the 80% bar) is concentrated in
fact-bearing chunks whose names the question does *not* speak verbatim. Two
follow-up hypotheses for reaching them were measured and **falsified**:

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

- **Go service** — `economy` PASS (42.1%, 0 miss), `gate` G2/G3 PASS (93%).
  The result that justifies the pivot; carries the documented cross-shape
  trade-offs on one hard task.
- **Python lib** — `economy` **PASS** (82.3%, 0 miss); `gate` G2 PASS, G3
  **FAIL (67%)** against the 80% bar — up from 13% in four measured steps.
  The remaining misses need evidence the question doesn't name verbatim:
  real embeddings on cold repos is the unblocked-by-code, blocked-by-key
  direction (no `OPENAI_API_KEY` in this environment).
- **TS/JS toy** — `economy` FAIL (small-file inversion), `gate` G2/G3 PASS.

## Real TS frontend: vuejs/core (2026-07-04)

The toy-repo inversion said nothing about the TS *shape*, so a real corpus
landed: [`g5_fixtures/vue/`](g5_fixtures/vue) — 6 questions, 16 grep-verified
identifiers against a fresh vuejs/core checkout (599 indexed files).
Reproduce with:

```
git clone --depth 1 https://github.com/vuejs/core /tmp/vue && neurofs scan /tmp/vue
neurofs economy --repo /tmp/vue --fixtures-dir docs/g5_fixtures/vue
```

Findings, in measured order:

1. **Baseline: WARN — economics hold (64.2% reduction), recall is the gap
   (44% overall, 2/6 search misses).** The same profile click had before its
   fixes. The TS shape is not inverted; the toy result was a size artefact.
2. **Landed — nested-closure chunking.** vuejs/core hides its renderer API
   inside one factory: `baseCreateRenderer` was a single 15,272-token chunk
   (lines 335–2472) whose inner `const mountComponent = (...)` closures were
   invisible to search. The JS chunker now emits `parent.closure` chunks for
   function-expression assignments and named functions nested one level deep
   in large (≥40-line) function bodies, each claimed by its innermost parent
   (heuristic decl-end detection can make a top-level `let`'s range swallow
   later functions; without the innermost rule every bogus parent re-emits
   the same closure). 174 nested chunks on vue; economy 64.2% → **67.2%**;
   Go and Python shapes unchanged (70.9% / 82.9% same-tree).
3. **Falsified — tiny-chunk downrank.** With closures indexed, the remaining
   misses trace to 1–4-line stubs and type aliases (`export const Vue`,
   `type Renderer`, `type Component`) winning `symbol_exact` on ordinary
   question words and crowding the 3-per-file diversity cap — the
   `mcp-tools-list` failure shape, reproduced on TS. A multiplicative
   downrank for sub-40-token chunks (keep=0.7) was A/B-tested across all
   three corpora with `learn eval`: click recall **66.7% → 53.3%**, tokens up
   on every shape, vue recall unchanged. Tiny stubs are *cheap* tokens and
   sometimes *are* the answer. The knob ships neutral (`tiny_chunk_keep`
   = 1.0, tunable) so the weight tuner can re-explore it as fixtures grow.
4. **Out-of-sample validation of the applied weights.** The multi-corpus
   tuned weights (trained on NeuroFS + click) were tested on vue before any
   vue-specific work: recall identical to defaults (same 2 misses), economy
   equivalent (62.8% vs 64.2%). The applied tune generalizes to a shape it
   never saw — the check the first (reverted) single-repo tune failed.

5. **Landed — `impl_kind`, born neutral, switched on by evidence.** The
   kind-aware fix shipped as a signal with weight **0** (inert), because the
   scoring history here is a graveyard of hand-picked weights. Two pieces
   made it adoptable: chunks carry their kind (func/method/nested_func vs
   type/const/default-stub declarations), and the tuner gained fixed probe
   values for zero weights (multiplicative steps can never move a 0). The
   3-corpus tune adopted it at 0.5 and vue recall jumped **50% → 66.7% on
   the search surface**; economy verdict **WARN → PASS (77.0%, miss rate
   33% → 17%)**. Cost, per the usual policy: Go economy recall 92% → 89%
   (still 0 miss, PASS), G3 97% → 94%. Classes are deliberately not
   "implementation" — click's fixtures need class headers competing evenly.

With that, **all three real shapes PASS the economy gate** — the first time
since cross-shape measurement began. The remaining vue miss
(`component-setup`) still loses to compat-layer declaration noise
(`installCompatInstanceProperties.set` and friends); candidate next steps
are compat-path awareness or real embeddings, both measurable.

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

# G5 — Cross-shape sanity

The pivot gate's G5 asks whether the engine holds across repository shapes, not
just on this Go service. The final mechanical run uses one binary, one weights
file, deterministic mock embeddings, pinned commits, hashed fixture sets, and
fresh indexes. Its immutable record is
[`audit/g5/2026-07-29T153550Z-mechanical-v2-target-bound.json`](../audit/g5/2026-07-29T153550Z-mechanical-v2-target-bound.json);
the nine retained command reports are under
[`audit/g5/reports/2026-07-29T153550Z-mechanical-v2-target-bound/`](../audit/g5/reports/2026-07-29T153550Z-mechanical-v2-target-bound/).

| Shape | Pinned commit | indexed files | `economy` reduction | overall recall / miss | `bench` top-3 | G2 | G3 |
|---|---|---:|---|---|---:|---|---|
| Go service — NeuroFS | `2d347b01b1508a6766164e603b2591f92a2ef816` + source-tree hash | 221 | **PASS · 63.7%** | 76.9% / 7.7% | 66.7% | PASS · 13 bundles | **PASS · 87.8%** |
| Python lib — [pallets/click](https://github.com/pallets/click) | `00e592cea702e0b2caa0dee42489fdb1c22cd845` | 128 | **PASS · 71.5%** | 66.7% / 0% | 100% | PASS · 5 bundles | **PASS · 80.0%** |
| TS frontend — [vuejs/core](https://github.com/vuejs/core) | `b5f8518379b77c3b62a7a9d2b52f6c76cda09bd5` | 600 | **PASS · 72.8%** | 72.2% / 0% | 100% | PASS · 6 bundles | **PASS · 83.3%** |

G5 therefore evaluates to **PASS**. Click sits exactly on the 80% G3 boundary;
the evaluator compares ratios with a `1e-12` tolerance so the mathematically
exact mean is not rejected as `0.7999999999999999`. G1 remains `SKIP`: these
are agent-run mechanical measurements, not human usefulness ratings.

The immutable JSON is the source of truth for the exact binary, weights,
engine-source and target-source SHA-256 identities. Keeping those values in one
place avoids a documentation-only copy drifting from the evidence it describes.

Schema v2 does not trust summary numbers in isolation. It verifies the retained
per-task economy rows, every G3 fact result, G2's exact one-bundle-per-fixture
attestations, the raw report hashes, target commits/remotes, weights and fixture
sets. `make build` also stamps the effective engine source-tree hash into the
binary; `--g5-attest` rejects an unstamped binary or a binary built from a
different checkout.

To reproduce a shape, check out the recorded target commit, copy the recorded
weights to `<target>/.neurofs/weights.json`, and run:

```bash
ENGINE=/path/to/NeuroFS
TARGET=/path/to/pinned/target
FIXTURES="$ENGINE/docs/g5_fixtures/click" # use audit/facts for NeuroFS
REPORT_DIR=/path/to/retained/reports

cd "$ENGINE" && make build
export NEUROFS_EMBEDDING_PROVIDER=mock
unset NEUROFS_MOCK_SEMANTIC
"$ENGINE/bin/neurofs" scan "$TARGET"
"$ENGINE/bin/neurofs" economy --repo "$TARGET" --fixtures-dir "$FIXTURES" \
  --g5-attest --g5-engine-root "$ENGINE" --out "$REPORT_DIR/economy.json"
"$ENGINE/bin/neurofs" gate --repo "$TARGET" --fixtures-dir "$FIXTURES" \
  --g5-attest --g5-engine-root "$ENGINE" --out "$REPORT_DIR/gate.json"
```

> Use `make g5-remeasure` and then, after committing, `make g5-verify`. They
> encode this whole procedure including both traps below, so the manual steps
> that follow are the contract they implement rather than something to run by
> hand. Re-measure after **any** commit touching code, docs or dependencies:
> the digest covers all of them.

**Measure the Go shape from a clean checkout, not your working tree.** G5
binds `target_source_tree_sha256` to the *indexed* tree, and `audit/` is
indexed while `audit/bundles/` and `audit/records/` are gitignored. A working
tree that has run the tool accumulates local-only bundles and records there,
so its indexed tree — and therefore the attested hash — cannot be reproduced
by CI or by anyone else. The evidence still verifies on the machine that
produced it, which is exactly what makes the mistake easy to miss. Note that
the dirty-checkout allowance for `go_service` does not rescue this: it permits
uncommitted *tracked* edits, while the hash covers everything indexed.

The attested gate is a single pass: G2 is derived from the fresh bundles
produced for G3 and therefore rejects `--bundles-dir`. External canonical
checkouts must be clean, including ignored files other than `.neurofs`.
Run `bench --out` with the matching `docs/g5_bench/<shape>.json` for the
supplementary file-ranker report.

## Historical development (superseded measurements)

The sections below preserve the path to the final result, including failed
experiments and corrections. Their older numbers are historical, not the
current G5 verdict above.

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

G1 measures *human* usefulness via `neurofs task --rate`
(`.neurofs/quality.jsonl`, gitignored). Agent or synthetic assessments do not
qualify for this criterion. On a fresh checkout, G1 is therefore `SKIP`; because
the overall gate is conjunctive, that also keeps the overall verdict at `SKIP`
unless another criterion yields `WARN` or `FAIL`. The mechanical G5 result can
pass independently, but the honest pivot-readiness verdict remains pending
genuine human use.

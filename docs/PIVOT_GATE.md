# Pivot-Readiness Gate

NeuroFS is being built local-first on purpose. The hosted, no-install web
product comes **after** the local product is good enough to deserve broader
delivery — not before. This document defines what "good enough" means in
concrete, measurable terms.

The gate exists for one reason: **so we know if NeuroFS is genuinely
improving or if we are fooling ourselves.** Every iteration on the engine
should leave at least one criterion measurably stronger or stay neutral.
Regressions are visible.

Run the gate with:

```
neurofs gate
```

It does not modify source files, the index, or audit evidence: it loads the
artefacts the product already produces, scores them, and prints a per-criterion
verdict plus an overall verdict. G3 recomputation does refresh
`.neurofs/task/` cache files; use `--skip-fixtures` when even that cache write
is undesirable. JSON is available via `--json` and can be written atomically
with `--out <path>`. No LLM calls are made.

---

## Verdict semantics

Each criterion returns one of four verdicts:

| Verdict | Meaning |
|---|---|
| `PASS` | Measured and within thresholds. |
| `WARN` | Measured and within hard thresholds, but a soft signal warrants attention. |
| `FAIL` | Measured and outside thresholds. Engine work needed before pivot. |
| `SKIP` | Not enough data to evaluate. Not a failure — just "produce more signal first". |

The overall verdict aggregates by priority: any `FAIL` ⇒ overall `FAIL`;
otherwise any `WARN` ⇒ overall `WARN`; otherwise any unevaluated criterion
(`SKIP`) keeps the overall result at `SKIP`. Overall `PASS` means every
criterion was measured and passed.

Process exit code: `1` only on overall `FAIL`. `WARN` and `SKIP` exit `0`
because they are not blocking; CI that wants to block on `WARN` should
parse `--json` explicitly.

---

## G1 — Real-use signal

**Source.** `.neurofs/quality.jsonl`, appended by `neurofs task --rate`.
Entries record `source: "human"`; explicitly agent-generated or synthetic
ratings are ignored. Legacy entries remain eligible unless their comment
identifies them as non-human, so historical ledgers never need rewriting.

**Question.** Are humans actually using this and finding it useful?

**Verdict.**
- `SKIP` if fewer than `MinSamples` (default 10) ratings are present.
- `FAIL` if yes-rate < `MinYesRate` (default 0.8).
- `PASS` otherwise.

**Why these defaults.** Ten ratings is the smallest sample where 80%
yes-rate is meaningful (anything under, the binomial CI is too wide).
Eighty percent leaves room for the genuinely-hard 20% of queries while
still demanding the tool earns its place in the workflow.

**How to move it.** Use `task --rate` for every real query you ask. Even
one entry per workday accumulates the signal in two weeks.

---

## G2 — Budget discipline

**Source.** `audit/bundles/*.json`, persisted by `task` (via the cache)
and by `pack --save-bundle`.

**Question.** Do bundles respect the budget the user asked for?

**Verdict.**
- `SKIP` if no bundles are present.
- `FAIL` if any JSON snapshot is malformed, has a non-positive budget, or has
  a negative used-token count; invalid evidence is never counted as a bundle.
- `FAIL` if **any** bundle has `tokens_used > tokens_budget` (overshoot).
- `PASS` otherwise.

The criterion **does not require high utilisation.** After `RepExcerpt`
landed, a small bundle is often the better bundle: top-ranked TS/JS/Python
files contribute as targeted excerpts (~150 tokens) instead of sprawling
signatures (~400 tokens). Penalising that as "wasted budget" would push
the engine in the wrong direction.

**Reported numbers.** `median_util` and `p95_util` are surfaced for
operator visibility. `WARN` post-processing applies (see below).

**WARN post-processing.** If G2 is `PASS` AND median utilisation < 0.5
AND G3 is `FAIL`, G2 is downgraded to `WARN`. Rationale: low utilisation
*alone* is fine. Low utilisation *correlated with poor fact recovery* is
a signal that the packager is leaving useful context on the table — that
is the case worth flagging.

**How to move it.** A `FAIL` here is a real bug — investigate the
offending bundle. The CLI names the first offender in the detail string.

---

## G3 — Fact recovery

**Source.** `audit/facts/*.json`, hand-written or promoted fixtures of the form:

```json
{
  "question": "How does the ranker score filename matches?",
  "expects_facts": ["weightFilename", "filename_match", "scoreFile"]
}
```

Fixtures are append-only evidence. When code evolution makes one obsolete,
add a complete correction in the same directory instead of editing or deleting
the original:

```json
{
  "question": "How does the session pool detect database or WAL changes?",
  "expects_facts": ["sessionFor", "currentIndexRevision", "SearchShared"],
  "source": "correction",
  "supersedes": "learned-13c0047c29b4.json"
}
```

`supersedes` must be the exact `.json` basename of one older fixture in the
same directory. Both `gate` and `learn` evaluate only the active end of each
correction chain while retaining every historical file. They fail closed on a
missing or path-shaped target, self-reference, cycle, or two corrections that
claim the same target; an active fixture with no expected facts is also
invalid.

For each fixture, the gate runs `taskflow.Run(force=true)` against the
current index and counts which expected facts appear (case-insensitive
substring) in the concatenated bundle content. Same scorer
`audit replay --facts-file` uses.

**Question.** When the user asks about a real piece of the codebase,
does the bundle actually contain the names/concepts/identifiers needed
to ground the answer?

**Verdict.**
- `SKIP` if fewer than `MinFixtures` active fixtures are present (default 5).
- `FAIL` if mean recall < `MinMeanRecall` (default 0.8).
- `PASS` otherwise.

**Reported numbers.** `mean_recall`, `perfect` (count of 1.0 recall),
`worst_recall`. On `FAIL`, the worst fixture's question is named.

**How to write good fixtures.**
- Use **identifier-shaped** facts: `weightFilename`, `RepExcerpt`,
  `selectFragment`. These survive whether the file appears as full code,
  signature, or excerpt — symbol names show up in all three.
- Avoid **value** facts (`3.0`, `600`, `0.85`) — signatures replace
  values with `...` so a value-shaped fact would fail unjustly.
- 3-6 facts per fixture is the sweet spot. Fewer ⇒ noisy recall; more
  ⇒ a single missing fact tanks the recall ratio.

**How to move it.** When a fixture fails, inspect the bundle: is the
right file ranked top-3 (ranking issue)? Is it included but as a bare
signature (extraction issue)? Is it included but the relevant body is
elided (excerpt-extractor issue)?

---

## G4 — Replay drift

**Sources.** Pooled automatically from every drift observation available:

1. **Records** — persisted replay verdicts under `audit/records/*.json`
   (written by `audit replay` and the UI).
2. **Pairs** — saved responses under `audit/responses/*` stem-paired with
   their bundle snapshot in `audit/bundles/` (`responses/x.md` ↔
   `bundles/x.json`). Each pair is **re-scored** with `audit.DetectDrift`
   against the bundle bytes on disk, so the gate measures the history
   itself rather than trusting a verdict persisted earlier. The recompute
   agrees exactly with `audit replay` (validated on the seed pair: 88.0%
   both ways). Every response is evidence: an orphan response or malformed
   matching bundle makes the gate fail closed instead of silently shrinking
   the sample set.
3. **Grounding ledger** — response-kind events from the continuous
   grounding hook (`audit/grounding.jsonl`, see `neurofs ground`).
   Edit-kind events are deliberately excluded: drift over an edit can be
   legitimate new code.

**Question.** Across everything we can replay, do model responses stay
grounded in the bundles they were given?

**Verdict.**
- `SKIP` if fewer than `MinSamples` samples exist (default 3); one or two
  observations are reported but cannot produce a stable median verdict.
- `FAIL` if **median** drift rate > `MaxMedianDrift` (default 0.15). The
  detail always carries the mean, the per-source counts, and names the
  worst sample.
- `PASS` otherwise.

**Why the median.** Drift measures grounding discipline, not hallucination
alone: a *design-plan* response legitimately names files and symbols that
do not exist yet, and reads as high drift (the seed pair, a plan for
`audit diff`, scores 88%). A mean lets one such sample fail the criterion
until diluted by sheer volume — measured on a real history of three
grounded implementation responses plus that plan (0%, 0%, 13%, 88%), the
mean reads 25.3% (FAIL) while the median reads 6.5% (PASS). The median
asks the right question — "is the *typical* response grounded?" — and the
mean and worst sample stay in the report so an operator still sees the
outliers. The minimum of three samples prevents a single response from
declaring the history clean or broken.

**Calibration note.** The drift detector rewards verbatim identifiers:
prose forms of bundle content ("TypeScript" for `models.LangTypeScript`,
`QueryTerms` for `opts.QueryTerms`) and meta-references to artefact
filenames read as drift. Grounded agent responses should cite identifiers
exactly as they appear in the context — which is the citation discipline
the bundle instructions already ask for.

**How to move it.** Every `audit replay`, every complete saved
bundle+response pair, and every loop turn with the grounding hook enabled adds
a sample. Keep the pair stems identical; an orphan is invalid audit evidence
and stops the gate.

**Known sharp edge.** A response that exists both as a persisted record and
as a bundle+response pair contributes two samples (one per source). With a
median verdict the skew is bounded, but prefer one pipeline per response:
either persist the replay record or save the pair, not both.

---

## G5 — Cross-shape sanity

**Source.** The newest immutable run under `audit/g5/*.json`. Schema v2 records
the exact repository commits, fixture-set hashes, binary and weights hashes,
the actor, deterministic provider/search/budget settings, report hashes, and
the mechanical `economy`, G2, and G3 results for every shape. Raw reports live
under `audit/g5/reports/<run>/`. The loader rejects malformed, oversized,
duplicate, symlinked, non-regular, or partially valid evidence instead of
silently dropping it.

The verifier recalculates economy summaries from every retained task row and
G3 from every retained fact result. It also verifies each raw report hash,
canonical fixture coverage and the exact fresh bundle generated for each G3
fixture. In attested mode G2 is therefore measured in the same gate invocation
as G3, not from an unrelated `--bundles-dir`.

`make build` stamps the effective source-tree hash into the executable.
`economy --g5-attest` and `gate --g5-attest` require
`--g5-engine-root`; an unstamped binary, an old binary, or a source checkout
whose hash differs from the embedded provenance is rejected. External
canonical repositories must match their pinned commit and remote and be clean,
including ignored files other than `.neurofs`.

**Question.** Does the gate hold not just on this repo (a Go service)
but on at least three repository shapes — Go service, TS/JS frontend,
Python lib?

**Verdict.**

- `SKIP` if no cross-shape run has been recorded.
- `FAIL` if the newest run uses a historical schema, no longer matches the
  current source tree, stamped binary provenance, weights, canonical fixture
  sets, or retained raw reports; if a required shape is absent/duplicated; or
  if any shape fails:
  - economy `PASS`, mean iso-recall reduction ≥ 25%, miss rate < 1/3;
  - G2 `PASS`, at least one real bundle, zero overshoots;
  - G3 `PASS`, at least five fixtures, mean recall ≥ 80%.
- `PASS` when the Go service, Python library, and TypeScript frontend all
  satisfy those thresholds in the same run.

G5 is deterministic mechanical evidence, so `actor_kind: "agent"` is valid
and explicit. That does **not** relax G1: usefulness ratings remain human-only.
The current run and exact commits are documented in
[`phase_g5_cross_shape.md`](phase_g5_cross_shape.md).

---

## When the gate has passed

NeuroFS is ready to consider the hosted pivot when **all of**:

- G1 = `PASS`: at least 10 eligible human ratings with a yes-rate of at
  least 80%
- G2 = `PASS` (no overshoot ever)
- G3 = `PASS` on at least 5 distinct fixtures
- G4 = `PASS`: at least 3 pooled drift samples with median drift no greater
  than 15%
- G5 = `PASS` on at least 3 repository shapes

Those are the machine-enforced conditions. The current evaluator reads the
full available G1 and G4 ledgers; it does not implement consecutive-week
streaks or a rolling 30-day filter. A team may still use those periods as an
operational observation policy, but they must not be presented as conditions
that `neurofs gate` verifies.

Until that happens, every iteration goes into the engine — ranking,
fragment extraction, representation choice, prompt shape — not into
hosted UX, auth, or web plumbing.

The pivot is not the product. The pivot is the packaging of a product
that already works.

---

## What the gate is NOT

- Not a benchmark. Recall is not "model accuracy" — it just confirms the
  expected identifiers reach the bundle.
- Not a CI gate against the engine. Use `bench --min-top3` for that —
  bench measures ranker precision against a synthetic corpus and is the
  right place to catch ranking regressions.
- Not a substitute for human judgement. G1 is the only criterion that
  measures actual usefulness; the others measure the mechanical
  preconditions for usefulness.

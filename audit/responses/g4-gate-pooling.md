<!-- response by the loop agent, written from the saved bundle -->

The gate represents every drift observation as a `DriftSample`
(internal/gate/gate.go:333-339): an `Origin` tag ("record", "pair", or
"grounding"), a human `Label` (question, pair stem, or session id), and the
`Rate` produced by `audit.DetectDrift` — unknown over known plus unknown.

The three sources feed that one shape. Persisted records are projected by
`SamplesFromRecords` (internal/gate/gate.go:341-348), which maps each
`audit.AuditRecord` to a sample carrying `r.Question` and `r.Drift.Rate`.
Saved histories are walked by `CollectPairDrift`
(internal/gate/gate.go:350-387): it reads `responsesDir`, pairs each response
by file stem with a bundle snapshot in `bundlesDir`, and re-scores the pair
with `audit.DetectDrift` against the bundle bytes on disk — the gate measures
the history itself rather than trusting a verdict persisted earlier. Responses
without a matching bundle are skipped; there is nothing hermetic to score them
against. A missing responses directory returns cleanly via `os.IsNotExist`.

The grounding ledger contributes through the same event model the hook
writes: `Event.Grounded` (internal/grounding/grounding.go:63-75) is the
per-event bar — for `KindEdit` the file had to be in context, for
`KindResponse` it requires `GroundedRatio >= 0.5` with `DriftRate` not
dominant — and `sampleUnknown` (internal/grounding/grounding.go:246-255)
caps the drifted identifiers carried per event, appending `UnknownPaths`,
`UnknownAPIs`, and `UnknownSymbols` up to a limit. The supervisor view rolls
events into `Aggregate` (internal/grounding/grounding.go:187-203), with
`MeanRespDrift` and `MeanEditDrift` tracked separately — edit drift can be
legitimate new code, which is why only response-kind events feed the gate.

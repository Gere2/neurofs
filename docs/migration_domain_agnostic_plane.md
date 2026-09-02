# Migration map — code plane → domain-agnostic plane

This map is ordered so each step is shippable, testable without Raíz, and
reversible. It does not authorize connectors, hosted auth, or a rewrite.

Companion: [ADR 0001](adr/0001-domain-agnostic-context-verification-plane.md).

## Guardrails for every phase

- `go test ./...` stays green.
- `neurofs gate` criteria are not edited to pass.
- Ledgers under `.neurofs/` and `audit/` stay append-only; correct with new
  records, never by rewrite.
- Code retrieval (`neurofs_context`, `neurofs_search`, pack/task, G3/G5) stays
  the production path; adapters wrap it, they do not replace it in-place.
- No Raíz / Firestore / Gmail / ERP client until a later, explicit ADR.

## Current topology (code-shaped)

```
repo checkout
  → indexer/parser/chunker/storage
  → retrieval + ranking + packager
  → Bundle (paths, langs, excerpts)
  → MCP / CLI / orchestrator
  → audit + grounding + receipt.Repo
  → gate G1–G5
```

LLM completions used by orchestrate can return a silent mock when the API key
is missing. Embeddings may use an explicit local mock for indexing; that is a
different contract.

## Target topology (plane + adapters)

```
kernel (scope, evidence, epistemic, verify, PACR, fail-closed LLM)
    ├── adapter: code   (existing indexer → retrieval → bundle → gate)
    └── adapter: business (later; not in this repo yet)
              ↑ observations only; SoR stays outside
```

NeuroFS never owns canonical records. Adapters observe systems of record,
emit fragments with evidence pointers, and verify answers against those
pointers.

## Phase 0 — Kernel types only (this change)

**In:** `internal/kernel`, this map, ADR 0001.

**Out:** no MCP tools, no CLI flags, no orchestrator behavior change, no
receipt schema change, no Raíz.

| Deliverable | Test without Raíz |
|---|---|
| `Scope` required (org + tenant) | Reject empty org/tenant |
| `EpistemicStatus` closed set | Reject unknown / mixed use |
| `EvidenceRef` (system, locator, hash) | Confirmed facts require evidence |
| `Claim` / `Answer` verification | Answer with ungrounded fact fails |
| PACR state machine | Command without approval fails |
| `LLM` enterprise complete | Missing key → error, empty text |

**Done when:** `go test ./internal/kernel` proves the invariants; the rest of
the tree is unchanged in behavior.

## Phase 1 — Name the code adapter (no behavior change)

Wrap existing retrieval behind a kernel-facing interface in a new package
such as `internal/domain/code`, implemented by calling today’s
`retrieval.Search` / `taskflow` / `packager`. Public MCP names stay
`neurofs_context` / `neurofs_search`.

- Map `ContextFragment` → kernel fragment: `EvidenceRef{System:"git+fs", Locator: relPath#line, ContentHash}`.
- Map bundle inclusion reasons into `Claim` observations (not confirmed facts).
- Do not move files out of `internal/models`.

**Done when:** adapter tests use the sample repo / existing retrieval fixtures;
gate and economy numbers are unchanged.

## Phase 2 — Additive scope on new writes only

New kernel-backed writers stamp `Scope`. Existing JSONL schemas gain optional
fields (`organization_id`, `tenant_id`) with omitempty so old lines still
decode. Default local scope for a coding checkout:

- `organization_id`: `local`
- `tenant_id`: stable repo identity (module path or origin URL), not a raw
  absolute path if that would leak machine layout into shared ledgers.

Do not backfill by rewriting history.

**Done when:** a new grounding or ledger line can carry scope; G4 still reads
legacy lines.

## Phase 3 — Epistemic status in verification

Teach `audit.VerifyResponse` / claim entailment to *consume* kernel claims
when present, while keeping path:line decomposition for code responses.

- Bundle content and citations → `observation`.
- Model conclusions not in the bundle → `inference` (and drift if they look
  like facts).
- Human or verifier confirmation (existing `run_amendment`, human outcome,
  G1 ratings) → `confirmed_fact` with evidence (amendment id, receipt hash).

Do not treat token overlap as confirmation.

**Done when:** a unit test can distinguish the three statuses on a synthetic
answer; G4 median drift thresholds unchanged.

## Phase 4 — Receipts: source snapshot, not only git `Repo`

Today `run_receipt` requires `receipt.Repo`. Keep that required for the code
adapter. Add an optional parallel `source` object (kernel `EvidenceRef` +
domain id) for non-git runs. Coding receipts continue to fail closed if `Repo`
is missing.

PACR: orchestrate / Copilot adapters emit `Proposal` (plan), `Approval`
(policy allow / owner pin), `Command` (argv / provider call), `Receipt`
(existing `run_receipt`). Do not invent a second receipt ledger.

**Done when:** schema tests allow a non-git source *in addition to* git receipts;
code receipts still require `Repo`.

## Phase 5 — Enterprise LLM on the orchestrator path

Route `DefaultLLMClient` through `kernel.LLM`:

- Enterprise / missing key: fail closed (this is the invariant).
- Local coding walkthrough: opt-in `ModeSynthetic` with `AllowSynthetic`,
  still marked `Task.Synthetic` so tournament ledgers stay clean.

Do not make embeddings’ local mock the LLM default.

**Done when:** orchestrator tests cover both modes; keyless enterprise complete
never writes `[Mock` into a plan as a successful model response.

## Phase 6 — First business adapter (separate ADR)

Only after Phases 0–5. A business adapter will:

- Authenticate to an external SoR (Raíz or otherwise) **outside** NeuroFS.
- Pull observations (never copy the SoR).
- Emit fragments with `EvidenceRef` pointing at the SoR document.
- Verify answers against those refs.
- Use PACR for any mutating call.

Firestore, Gmail, and similar are **sources of evidence**, not NeuroFS
tables. This phase is explicitly not started now.

## What not to migrate

| Temptation | Why not |
|---|---|
| Make `FileRecord` a generic document | Breaks indexer, G3 identifier facts, G5 shapes |
| Put invoices into `chunks` | Turns NeuroFS into a SoR and pollutes ranking |
| Delete silent LLM mock without an opt-in | Breaks the documented local orchestrate walkthrough |
| Weaken G3/G5 so a business fixture “counts” | Gate is the code-product oracle |
| Rewrite `audit/records` to kernel JSON | Violates append-only history |
| Connect Raíz “just to see” | Out of scope until Phase 6 ADR |

## Rollback

Phase 0 is additive (`internal/kernel` + docs). Delete the package if the
bet is abandoned; nothing in the code adapter depends on it yet.
Later phases must keep optional fields so rollback is “stop writing new
fields”, not “rewrite ledgers”.

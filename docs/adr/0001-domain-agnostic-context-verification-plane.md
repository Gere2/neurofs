# ADR 0001 — Domain-agnostic context and verification plane

- Status: accepted
- Date: 2026-08-22
- Deciders: NeuroFS maintainers
- Does not change: `neurofs gate` criteria (G1–G5), code retrieval, or any system of record

## Context

NeuroFS is today “the context and verification plane for autonomous coding loops”.
That product is real, gated, and must keep working. The next architectural bet is
that the same plane — select, compress, cite, distinguish epistemic status, and
verify against evidence — is useful outside source repositories.

This ADR records how to become a **domain-agnostic** context and verification
plane **without a rewrite**, without connecting Raíz, Firestore, Gmail, or any
other enterprise system, and without NeuroFS becoming a system of record.

## Decision

1. Introduce a small kernel (`internal/kernel`) that names the plane’s contracts:
   tenant scope, evidence pointers, epistemic status, verifiable answers, the
   Proposal → Approval → Command → Receipt action path, and a fail-closed
   enterprise LLM client.
2. Treat the existing code stack (`indexer`, `retrieval`, `packager`, `audit`,
   `gate`, `receipt.Repo`, …) as the **code-domain adapter** already in
   production. Do not rename those packages or weaken their APIs in this slice.
3. Add future domains as adapters that implement the kernel contracts. They must
   not live in `internal/models` or `internal/retrieval`.
4. Keep NeuroFS a plane over other systems: it stores observations, inferences,
   verification receipts, and pointers. Canonical business or code records stay
   in their systems of record.

## Invariants (non-negotiable)

| Invariant | Kernel encoding |
|---|---|
| NeuroFS is not a system of record | Claims carry `EvidenceRef` (system + locator + content hash). Payload is optional cache, never the authority. |
| Facts must point at evidence | `Claim` with status `confirmed_fact` is invalid without at least one `EvidenceRef`. |
| Observation, inference, and confirmed fact are distinct | `EpistemicStatus` is a closed enum; mixing them is a validation error. |
| Every datum can be scoped by organization/tenant | `Scope` is required on kernel records. Local code uses an explicit local scope, never an implicit “no tenant”. |
| Important answers are verifiable against evidence | `Answer` lists claims; `VerifyAgainstEvidence` rejects answers that assert facts without evidence. |
| Future actions use Proposal → Approval → Command → Receipt | `Action` status machine; no skipped states. |
| Enterprise LLM fails closed | Missing credentials return `ErrMissingCredentials` and empty text. Silent mock is forbidden. Explicit synthetic mode is opt-in and never the enterprise default. |
| Code use case and gates stay | This ADR does not change G1–G5, fixtures, or retrieval scoring. |

## Coupling map (what is code-domain vs plane)

### Must stay in the code domain (do not genericize)

These types are correctly about repositories, languages, and files. A business
domain will not reuse them.

| Type / module | Why it is code-specific |
|---|---|
| `models.Lang`, `Symbol`, `FileRecord`, `Chunk`, `FileRelation` | AST, paths, imports, checksums of source files |
| `models.Representation` (`full_code`, `excerpt`, `signature`, …) | How source is packed into a token budget |
| `indexer`, `parser`, `project`, `packager/excerpt_*` | File walk, manifests, language extractors |
| `retrieval.Options.Repo`, git working-set boost | Indexed checkout + dirty tree |
| `ranking` filename/symbol/import weights | Code ranker |
| `gate` G3/G5, `audit/facts`, cross-shape repos | Identifier recall in source bundles |
| `receipt.Repo` (`base_commit`, tree hashes) | Git snapshot of a coding run |
| `audit.Citation` path:line, `ClaimKind` citation/symbol | Code citations |
| `grounding` edit-in-context | Agent edited a file it had in the bundle |
| `fsutil` git changes, confine-to-repo | Filesystem + git |

### Plane-shaped, but currently named and stored as code

These *ideas* belong in the kernel. Today they are fused to repo paths, file
lists, or git receipts. Extract types first; migrate storage later with
append-only additive fields.

| Today | Kernel concept | Coupling to strip later |
|---|---|---|
| `models.Bundle` + `ContextFragment` | Context pack of citable fragments | `RelPath` / `Lang` as the only locator |
| `models.InclusionReason` | Why a fragment was selected | Signal names are code-ranker specific; keep as domain metadata |
| `models.LedgerEntry` | Session observation | `Files`, `BundlePath` assume a repo |
| `audit.AuditRecord` | Grounding observation of an answer | Fragments are source excerpts |
| `audit.DriftReport` | Ungrounded identifiers | Path / API / symbol buckets |
| `audit.Claim` + `VerificationReport` | Verifiable claims vs evidence | Entailment is substring-in-bundle; sandbox is `go build`/`go test` |
| `receipt.Record` | Run receipt | `Repo` is required on every `run_receipt` |
| `receipt.ContextBundle` | Evidence of context served | Repo-relative `bundle_path` |
| `receipt.Provenance` / `Confidence` | Provenance ≠ estimated-as-observed | Already domain-agnostic; reuse |
| `runid.RunID` | Correlation of one controlled execution | Already domain-agnostic; reuse |
| `orchestrator.Task` / `Plan` | Decomposed work | `Repo` on `Plan`; `Synthetic` papers over silent LLM mock |
| `DefaultLLMClient.Complete` | LLM transport | Empty API key returns a `[Mock …]` completion (`nil` error) |
| `embeddings.Client` mock fallback | Local-first embeddings | Acceptable for **indexing** without an explicit cloud provider; **not** a template for enterprise LLM completions |

### Already fail-closed (keep; do not regress)

- `embeddings.Validate`: explicit cloud provider without a key errors; typos are not rewritten to mock.
- `gate` malformed evidence, orphan responses, fixture supersession cycles.
- `surface/copilot.PlanSelection`: unverifiable model restriction refuses auto-routing.
- `receipt` validation: absence never means zero; amendments never rewrite receipts.
- `runid`: conflicting identities error rather than silently preferring one.

### Not present today (kernel must introduce)

- Organization / tenant scope on any persisted type.
- Epistemic status as a first-class field (observation vs inference vs confirmed fact).
- Evidence pointers that are not file paths.
- Action state machine Proposal → Approval → Command → Receipt (CLI `cobra.Command` and `receipt.Record` are unrelated).
- Enterprise LLM client that cannot return a silent mock.

## Explicit non-goals (this slice and the next)

- No rewrite of indexer, retrieval, packager, MCP tools, or UI.
- No Raíz, Firestore, Gmail, CRM, or ERP connectors.
- No hosted multi-tenant product, auth, or billing.
- No change to `docs/PIVOT_GATE.md` thresholds or G5 shape requirements.
- No single-corpus `learn tune --apply`.
- NeuroFS will not store canonical customers, invoices, emails, or source-of-truth documents.

## Consequences

**Positive.** A business adapter can later supply fragments with `EvidenceRef`
(e.g. a Firestore document path + generation + hash) without teaching
`internal/ranking` about invoices. The code product remains the first adapter.
Enterprise completions have a tested fail-closed contract before any corporate
LLM is wired.

**Negative / deferred cost.** `receipt.Repo` stays required for coding-run
receipts until a later additive schema (optional `source` instead of only
`repo`). Orchestrator’s offline `[Mock]` path remains for the local coding
walkthrough until a follow-up routes enterprise mode through `kernel.LLM`.
Ledgers stay unscoped on disk; kernel `Scope` is the type new writers must use.

**Risks this ADR refuses.** Genericizing `FileRecord` into “document”, stuffing
business objects into SQLite `chunks`, or letting NeuroFS mint customer IDs.
Those would make NeuroFS a system of record and break the code gates.

## Follow-up order

See [migration map](../migration_domain_agnostic_plane.md). The only code
landed with this ADR is `internal/kernel` plus tests that do not import Raíz
or touch `internal/gate` criteria.

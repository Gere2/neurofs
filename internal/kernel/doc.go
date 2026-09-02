// Package kernel is the domain-agnostic context and verification plane.
//
// NeuroFS is not a system of record. This package holds the contracts every
// domain adapter (code today, business later) must obey: tenant scope,
// evidence pointers, epistemic status, verifiable answers, the Proposal →
// Approval → Command → Receipt action path, and a fail-closed enterprise LLM
// client.
//
// The production code indexer, retriever, packager and gate live outside this
// package and remain the code-domain implementation. Kernel types are additive;
// they do not replace models.Bundle or receipt.Repo in this slice.
package kernel

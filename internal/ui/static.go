// Package ui hosts the local NeuroFS web UI: an HTTP server that wraps the
// same primitives (scan, pack, replay, diff) the CLI exposes, so a user can
// run a full round without memorising commands. Nothing leaves the loopback
// interface — there is no outbound HTTP, no telemetry, no third-party JS.
package ui

import "embed"

// assets is the compiled-in copy of static/. Serving from embed avoids
// shipping a separate directory next to the binary and keeps the UI a single
// artifact — the same property as any other `neurofs` subcommand.
//
//go:embed static
var assets embed.FS

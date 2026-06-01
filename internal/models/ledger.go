// Package models defines the core data types for NeuroFS.
package models

import "time"

// LedgerEntry represents a single event recorded in the session memory.
type LedgerEntry struct {
	Timestamp  time.Time `json:"timestamp"`
	SessionID  string    `json:"session_id"`
	Query      string    `json:"query,omitempty"`
	BundleHash string    `json:"bundle_hash,omitempty"`
	Files      []string  `json:"files,omitempty"`
	Command    string    `json:"command,omitempty"`
	Outcome    string    `json:"outcome,omitempty"`
	Notes      string    `json:"notes,omitempty"`
}

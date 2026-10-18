// Copyright 2026 fxTunnel contributors. Licensed under Apache-2.0.
// Package state defines the persistence contract for embedded receipt workers.
package state

import "encoding/json"

type Record struct {
	Kind, ID, Phase string
	Value           any
}
type PendingJob struct {
	Kind     string `json:"kind"`
	ID       string `json:"id"`
	Phase    string `json:"phase"`
	Attempts int    `json:"attempts"`
	Next     string `json:"next,omitempty"`
	Error    string `json:"error,omitempty"`
}

// Store implementations must serialize writers across all server instances.
// Save must atomically persist every supplied record (including the outbox).
// Losing writer ownership must make all further Save/SetMeta calls fail, rather
// than transparently reconnecting. The engine saves intent before remote POSTs.
type Store interface {
	Meta(string) (string, error)
	SetMeta(string, string) error
	Get(kind, id string, out any) (bool, error)
	// List returns open workflows only (exclude done, sent and superseded).
	// Completed records must remain available through Get for deduplication.
	List(kind string) ([]json.RawMessage, error)
	Save(...Record) error
	Counts() (map[string]int, error)
}

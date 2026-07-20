// Package gateway implements the policy enforcement point of DESIGN §9:
// the single controlled door between agent sandboxes and model endpoints.
// Key custody, per-workspace routing, policy blocks, and the audit ledger —
// and deliberately nothing else (no translation, no caching, no rewriting).
package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"
)

// Event is one model-traffic audit record (the ledger's "model chapter" of
// the flight recorder, §11). Events are hash-chained: each carries the hash
// of its predecessor, so truncation or in-place edits are detectable.
type Event struct {
	Seq        uint64    `json:"seq"`
	Prev       string    `json:"prev"` // hex hash of previous event ("" for first)
	Time       time.Time `json:"time"`
	Workspace  string    `json:"workspace"`
	Route      string    `json:"route"`
	Model      string    `json:"model,omitempty"`
	Upstream   string    `json:"upstream,omitempty"`
	Outcome    string    `json:"outcome"` // "forwarded" | "blocked" | "upstream_error" | "auth_failed"
	Reason     string    `json:"reason,omitempty"`
	Status     int       `json:"status,omitempty"`
	ReqBytes   int64     `json:"req_bytes,omitempty"`
	RespBytes  int64     `json:"resp_bytes,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Hash       string    `json:"hash"` // hex sha256 over the event with Hash=""
}

// Ledger is an append-only, hash-chained JSONL file. Local mode writes it to
// disk; team/enterprise mode ships the same lines to the org's SIEM (§11).
type Ledger struct {
	mu   sync.Mutex
	f    *os.File
	seq  uint64
	prev string
}

// OpenLedger opens (or creates) the ledger file and, if it has prior
// entries, resumes the chain from the last one.
func OpenLedger(path string) (*Ledger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l := &Ledger{f: f}

	// Resume chain state from existing content.
	events, err := readEvents(path)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("ledger %s unreadable: %w", path, err)
	}
	if n := len(events); n > 0 {
		l.seq = events[n-1].Seq
		l.prev = events[n-1].Hash
	}
	return l, nil
}

func (l *Ledger) Close() error { return l.f.Close() }

// Append seals the event into the chain and writes it. The caller fills the
// domain fields; Seq, Prev, Time (if zero) and Hash are owned by the ledger.
func (l *Ledger) Append(e Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.seq++
	e.Seq = l.seq
	e.Prev = l.prev
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	e.Hash = ""
	h, err := hashEvent(e)
	if err != nil {
		return err
	}
	e.Hash = h

	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		return err
	}
	l.prev = e.Hash
	return nil
}

// Verify replays a ledger file and checks the chain: sequence continuity,
// prev-linkage, and every event's hash. Returns the number of valid events.
func Verify(path string) (int, error) {
	events, err := readEvents(path)
	if err != nil {
		return 0, err
	}
	prev := ""
	for i, e := range events {
		if e.Seq != uint64(i+1) {
			return i, fmt.Errorf("event %d: seq %d, want %d", i, e.Seq, i+1)
		}
		if e.Prev != prev {
			return i, fmt.Errorf("event %d: broken chain (prev %q, want %q)", i, e.Prev, prev)
		}
		want := e.Hash
		e.Hash = ""
		got, err := hashEvent(e)
		if err != nil {
			return i, err
		}
		if got != want {
			return i, fmt.Errorf("event %d: hash mismatch — ledger tampered or corrupt", i)
		}
		prev = want
	}
	return len(events), nil
}

func hashEvent(e Event) (string, error) {
	b, err := json.Marshal(e) // Hash field must be "" when called
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func readEvents(path string) ([]Event, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var events []Event
	dec := json.NewDecoder(bytes.NewReader(b))
	for dec.More() {
		var e Event
		if err := dec.Decode(&e); err != nil {
			return events, fmt.Errorf("malformed ledger line after %d events: %w", len(events), err)
		}
		events = append(events, e)
	}
	return events, nil
}

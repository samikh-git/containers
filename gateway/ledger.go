// Package gateway implements the policy enforcement point of DESIGN §9:
// the single controlled door between agent sandboxes and model endpoints.
// Key custody, per-workspace routing, policy blocks, and the audit ledger —
// and deliberately nothing else (no translation, no caching, no rewriting).
package gateway

import (
	"bytes"
	"crypto/hmac"
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
	Seq          uint64    `json:"seq"`
	Prev         string    `json:"prev"` // hex hash of previous event ("" for first)
	Time         time.Time `json:"time"`
	Workspace    string    `json:"workspace"`
	Route        string    `json:"route"`
	Model        string    `json:"model,omitempty"`
	Upstream     string    `json:"upstream,omitempty"`
	Outcome      string    `json:"outcome"` // "forwarded" | "blocked" | "upstream_error" | "auth_failed"
	Reason       string    `json:"reason,omitempty"`
	Status       int       `json:"status,omitempty"`
	ReqBytes     int64     `json:"req_bytes,omitempty"`
	RespBytes    int64     `json:"resp_bytes,omitempty"`
	DurationMS   int64     `json:"duration_ms,omitempty"`
	InputTokens  int64     `json:"input_tokens,omitempty"`  // prompt / input (when parseable)
	OutputTokens int64     `json:"output_tokens,omitempty"` // completion / output
	Hash         string    `json:"hash"`                    // hex sha256 over the event with Hash=""
}

// Ledger is an append-only, hash-chained JSONL file. Local mode writes it to
// disk; team/enterprise mode ships the same lines to the org's SIEM (§11).
//
// On tamper-evidence, precisely: an unkeyed SHA-256 chain detects truncation
// and accidental corruption, but not a motivated editor — anyone who can
// write the file can rewrite history and recompute every hash, and Verify
// would report the chain intact. Setting a key (GATEWAY_LEDGER_KEY) switches
// the chain to HMAC-SHA256, so forging it requires the key and not merely
// write access to the file. Keep the key somewhere the ledger's writer is
// not, or it is only a speed bump.
type Ledger struct {
	mu   sync.Mutex
	f    *os.File
	key  []byte
	seq  uint64
	prev string
}

// LedgerKeyEnv names the environment variable holding the chain key.
const LedgerKeyEnv = "GATEWAY_LEDGER_KEY"

// OpenLedger opens (or creates) the ledger file and, if it has prior
// entries, resumes the chain from the last one. The chain is keyed when
// GATEWAY_LEDGER_KEY is set.
func OpenLedger(path string) (*Ledger, error) {
	return OpenLedgerWithKey(path, []byte(os.Getenv(LedgerKeyEnv)))
}

// OpenLedgerWithKey is OpenLedger with an explicit chain key ("" for the
// unkeyed, corruption-evident-only chain).
func OpenLedgerWithKey(path string, key []byte) (*Ledger, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l := &Ledger{f: f, key: key}

	// Resume chain state from existing content.
	events, err := ReadEvents(path)
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
	h, err := hashEvent(e, l.key)
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
// Uses GATEWAY_LEDGER_KEY when set — verifying a keyed chain without the key
// (or an unkeyed one with it) fails at the first event, as it should.
func Verify(path string) (int, error) {
	return VerifyWithKey(path, []byte(os.Getenv(LedgerKeyEnv)))
}

// VerifyWithKey is Verify with an explicit chain key.
func VerifyWithKey(path string, key []byte) (int, error) {
	events, err := ReadEvents(path)
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
		got, err := hashEvent(e, key)
		if err != nil {
			return i, err
		}
		if got != want {
			return i, fmt.Errorf("event %d: hash mismatch — ledger corrupt, edited, or sealed with a different %s", i, LedgerKeyEnv)
		}
		prev = want
	}
	return len(events), nil
}

// hashEvent seals an event. With a key the chain is an HMAC chain, which
// cannot be recomputed by someone who only has the file; without one it is a
// plain digest, which can.
func hashEvent(e Event, key []byte) (string, error) {
	b, err := json.Marshal(e) // Hash field must be "" when called
	if err != nil {
		return "", err
	}
	if len(key) > 0 {
		mac := hmac.New(sha256.New, key)
		mac.Write(b)
		return hex.EncodeToString(mac.Sum(nil)), nil
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// ReadEvents loads every event from a ledger file. Missing files yield an
// empty slice (not an error) so callers can summarize a ledger that has not
// been written yet.
func ReadEvents(path string) ([]Event, error) {
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

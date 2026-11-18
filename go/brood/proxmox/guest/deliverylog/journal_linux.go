// Package deliverylog retains bounded delivery identities across guest restarts.
// An uncertain session write never becomes permission to repeat it.
package deliverylog

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	MaxEntries      = 4096
	MaxAttempts     = 8
	MaxPayloadBytes = 1 << 20
	recordBytes     = 512
	journalBytes    = (1 + MaxEntries*MaxAttempts*2) * (recordBytes + 1)
	journalName     = "delivery.jsonl"
)

var (
	ErrConflict    = errors.New("delivery identity or attempt conflicts with retained history")
	ErrFull        = errors.New("delivery journal is full; retained identities cannot be evicted")
	ErrAttempts    = errors.New("known not-sent delivery retry budget exhausted")
	ErrScope       = errors.New("delivery journal belongs to another actor incarnation")
	ErrCorrupt     = errors.New("delivery journal is incomplete or invalid; history was not discarded")
	ErrUnavailable = errors.New("delivery journal unavailable")
	ErrClosed      = errors.New("delivery journal is closed")
)

// Scope binds retained identities to one supervisor-owned actor incarnation.
type Scope struct {
	ActorUID    string
	Incarnation int64
}

// AdmissionState distinguishes a new durable reservation from retained outcomes.
type AdmissionState string

const (
	New             AdmissionState = "new"
	Duplicate       AdmissionState = "duplicate"
	Unknown         AdmissionState = "unknown"
	Absent          AdmissionState = "absent"
	NotSentRecorded AdmissionState = "not_sent"
)

// Outcome is root-observed transport evidence, never an actor's report.
type Outcome string

const (
	Delivered   Outcome = "delivered"
	NotSent     Outcome = "not_sent"
	Unconfirmed Outcome = "unconfirmed"
)

// Ticket identifies only the newly reserved attempt allowed to settle a write.
type Ticket struct {
	JournalID  string
	Scope      Scope
	ID, Digest string
	Attempt    int
}

// Admission gives a ticket only when a new write has been durably reserved.
type Admission struct {
	State  AdmissionState
	Ticket Ticket
}

type row struct {
	JournalID   string `json:"journalId,omitempty"`
	Version     int    `json:"version"`
	ActorUID    string `json:"actorUid,omitempty"`
	Incarnation int64  `json:"incarnation,omitempty"`
	ID          string `json:"id,omitempty"`
	Digest      string `json:"digest,omitempty"`
	Attempt     int    `json:"attempt,omitempty"`
	State       string `json:"state,omitempty"`
}

// Journal serializes processes through its file lock, not an in-memory cache.
// The caller supplies an existing private directory owned by the supervisor.
type Journal struct {
	mu         sync.Mutex
	file       *os.File
	scope      Scope
	capacity   int
	closed     bool
	journalID  string
	identified bool
	syncFileFn func() error
}

// Identifier binds admission and replay to this exact retained journal.
// An older journal without an identity remains readable but returns an empty ID.
func (j *Journal) Identifier() string { return j.journalID }

func validID(value string) bool {
	if len(value) < 1 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == ':' || c == '.') {
			return false
		}
	}
	return true
}
func privateFile(file *os.File, directory bool) bool {
	info, err := file.Stat()
	if err != nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return false
	}
	if directory {
		return info.IsDir() && info.Mode().Perm() == 0700
	}
	return info.Mode().IsRegular() && info.Mode().Perm() == 0600 && stat.Nlink == 1
}

// Open never resets a mismatched, empty-existing, partial, or corrupt journal.
// Successful creation syncs both the header and its directory before admission.
func Open(ctx context.Context, directory string, scope Scope, capacity int) (*Journal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(directory) {
		return nil, ErrUnavailable
	}
	if !validID(scope.ActorUID) || scope.Incarnation < 1 || capacity < 1 || capacity > MaxEntries {
		return nil, ErrScope
	}
	dir, err := os.OpenFile(directory, os.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer dir.Close()
	return OpenDirectory(ctx, dir, scope, capacity)
}

// OpenDirectory opens retained history relative to a pinned private directory.
// The caller owns dir and must establish that its parent namespace is trusted.
func OpenDirectory(ctx context.Context, dir *os.File, scope Scope, capacity int) (*Journal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(scope.ActorUID) || scope.Incarnation < 1 || capacity < 1 || capacity > MaxEntries {
		return nil, ErrScope
	}
	if dir == nil || !privateFile(dir, true) {
		return nil, errors.New("delivery journal directory must be private and supervisor-owned")
	}
	flags := unix.O_RDWR | unix.O_APPEND | unix.O_CLOEXEC | unix.O_NOFOLLOW
	fd, err := unix.Openat(int(dir.Fd()), journalName, flags|unix.O_CREAT|unix.O_EXCL, 0600)
	created := err == nil
	if errors.Is(err, unix.EEXIST) {
		fd, err = unix.Openat(int(dir.Fd()), journalName, flags, 0)
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	file := os.NewFile(uintptr(fd), journalName)
	if !privateFile(file, false) {
		file.Close()
		return nil, errors.New("delivery journal file must be private, regular, single-linked, and supervisor-owned")
	}
	j := &Journal{file: file, scope: scope, capacity: capacity}
	err = j.locked(ctx, func() error {
		if created {
			var nonce [32]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				return ErrUnavailable
			}
			if err := j.append(row{Version: 1, ActorUID: scope.ActorUID, Incarnation: scope.Incarnation, JournalID: hex.EncodeToString(nonce[:])}); err != nil {
				return err
			}
		}
		if _, err := j.read(); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return ErrUnavailable
		}
		if err := dir.Sync(); err != nil {
			return ErrUnavailable
		}
		return nil
	})
	if err != nil {
		file.Close()
		return nil, err
	}
	return j, nil
}

func (j *Journal) locked(ctx context.Context, fn func() error) error {
	timer := time.NewTicker(5 * time.Millisecond)
	defer timer.Stop()
	for !j.mu.TryLock() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer j.mu.Unlock()
	if j.closed {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	for {
		err := unix.Flock(int(j.file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			return ErrUnavailable
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer unix.Flock(int(j.file.Fd()), unix.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return err
	}
	// A complete append from another handle may still lack a successful sync.
	if err := j.syncFile(); err != nil {
		return ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

func decodeRow(data []byte) (row, error) {
	invalid := func() (row, error) { return row{}, ErrCorrupt }
	d := json.NewDecoder(bytes.NewReader(data))
	if token, err := d.Token(); err != nil || token != json.Delim('{') {
		return invalid()
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return invalid()
		}
		seen[name] = true
		switch name {
		case "version", "actorUid", "incarnation", "id", "digest", "attempt", "state", "journalId":
		default:
			return invalid()
		}
		value, err := d.Token()
		if err != nil || value == nil {
			return invalid()
		}
		if _, ok := value.(json.Delim); ok {
			return invalid()
		}
	}
	if end, err := d.Token(); err != nil || end != json.Delim('}') {
		return invalid()
	}
	if _, err := d.Token(); err != io.EOF {
		return invalid()
	}
	var record row
	d = json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if d.Decode(&record) != nil || record.Version != 1 {
		return invalid()
	}
	return record, nil
}

func (j *Journal) syncFile() error {
	if j.syncFileFn != nil {
		return j.syncFileFn()
	}
	return j.file.Sync()
}

func (j *Journal) read() (map[string]row, error) {
	if !privateFile(j.file, false) {
		return nil, ErrUnavailable
	}
	info, err := j.file.Stat()
	if err != nil {
		return nil, ErrUnavailable
	}
	if info.Size() == 0 || info.Size() > journalBytes {
		return nil, ErrCorrupt
	}
	var last [1]byte
	if _, err := j.file.ReadAt(last[:], info.Size()-1); err != nil || last[0] != '\n' {
		return nil, ErrCorrupt
	}
	if _, err := j.file.Seek(0, io.SeekStart); err != nil {
		return nil, ErrUnavailable
	}
	scanner := bufio.NewScanner(j.file)
	scanner.Buffer(make([]byte, recordBytes), recordBytes+1)
	entries := map[string]row{}
	header := false
	for scanner.Scan() {
		data := scanner.Bytes()
		if len(data) > recordBytes {
			return nil, ErrCorrupt
		}
		record, err := decodeRow(data)
		if err != nil {
			return nil, err
		}
		if !header {
			if record.ID != "" || record.Digest != "" || record.Attempt != 0 || record.State != "" || !validID(record.ActorUID) || record.Incarnation < 1 {
				return nil, ErrCorrupt
			}
			if record.ActorUID != j.scope.ActorUID || record.Incarnation != j.scope.Incarnation {
				return nil, ErrScope
			}
			if record.JournalID != "" {
				nonce, err := hex.DecodeString(record.JournalID)
				if err != nil || len(nonce) != 32 || hex.EncodeToString(nonce) != record.JournalID {
					return nil, ErrCorrupt
				}
			}
			if !j.identified {
				j.journalID, j.identified = record.JournalID, true
			} else if record.JournalID != j.journalID {
				return nil, ErrCorrupt
			}
			header = true
			continue
		}
		digest, err := hex.DecodeString(record.Digest)
		if record.JournalID != "" || !validID(record.ID) || err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != record.Digest || record.ActorUID != "" || record.Incarnation != 0 || record.Attempt < 1 || record.Attempt > MaxAttempts {
			return nil, ErrCorrupt
		}
		previous, exists := entries[record.ID]
		if !exists {
			if record.State != "pending" || record.Attempt != 1 || len(entries) >= MaxEntries {
				return nil, ErrCorrupt
			}
		} else {
			if previous.Digest != record.Digest {
				return nil, ErrCorrupt
			}
			terminal := record.State == string(Delivered) || record.State == string(NotSent) || record.State == string(Unconfirmed)
			if !(previous.State == "pending" && record.Attempt == previous.Attempt && terminal || previous.State == string(NotSent) && record.State == "pending" && record.Attempt == previous.Attempt+1) {
				return nil, ErrCorrupt
			}
		}
		entries[record.ID] = record
	}
	if scanner.Err() != nil || !header {
		return nil, ErrCorrupt
	}
	if len(entries) > j.capacity {
		return nil, ErrFull
	}
	return entries, nil
}

func (j *Journal) append(record row) error {
	data, err := json.Marshal(record)
	if err != nil || len(data) > recordBytes {
		return ErrCorrupt
	}
	data = append(data, '\n')
	info, err := j.file.Stat()
	if err != nil {
		return ErrUnavailable
	}
	if info.Size()+int64(len(data)) > journalBytes {
		return ErrFull
	}
	n, err := j.file.Write(data)
	if err != nil || n != len(data) {
		return ErrUnavailable
	}
	if err := j.syncFile(); err != nil {
		return ErrUnavailable
	}
	return nil
}

// Begin syncs reservation before returning New. Pending and uncertain records
// return Unknown without a ticket; only a durable confirmation returns Duplicate.
// Payload must be the complete canonical request, including routing and wait semantics.
func (j *Journal) Begin(ctx context.Context, id string, payload []byte) (Admission, error) {
	if !validID(id) || len(payload) == 0 || len(payload) > MaxPayloadBytes {
		return Admission{}, ErrConflict
	}
	hash := sha256.Sum256(payload)
	digest := hex.EncodeToString(hash[:])
	var admission Admission
	err := j.locked(ctx, func() error {
		entries, err := j.read()
		if err != nil {
			return err
		}
		previous, exists := entries[id]
		attempt := 1
		if exists {
			if previous.Digest != digest {
				return ErrConflict
			}
			switch previous.State {
			case string(Delivered):
				admission.State = Duplicate
				return nil
			case "pending", string(Unconfirmed):
				admission.State = Unknown
				return nil
			case string(NotSent):
				if previous.Attempt >= MaxAttempts {
					return ErrAttempts
				}
				attempt = previous.Attempt + 1
			default:
				return ErrCorrupt
			}
		} else if len(entries) >= j.capacity {
			return ErrFull
		}
		if err := j.append(row{Version: 1, ID: id, Digest: digest, Attempt: attempt, State: "pending"}); err != nil {
			return err
		}
		admission = Admission{State: New, Ticket: Ticket{Scope: j.scope, JournalID: j.journalID, ID: id, Digest: digest, Attempt: attempt}}
		return nil
	})
	if err != nil {
		return Admission{}, err
	}
	return admission, nil
}

// Finish never upgrades unknown history or settles another incarnation/attempt.
// It must receive the supervisor's observed result, not caller-selected evidence.
func (j *Journal) Finish(ctx context.Context, ticket Ticket, outcome Outcome) error {
	if outcome != Delivered && outcome != NotSent && outcome != Unconfirmed {
		return ErrConflict
	}
	if ticket.Scope != j.scope || ticket.JournalID != j.journalID {
		return ErrScope
	}
	return j.locked(ctx, func() error {
		entries, err := j.read()
		if err != nil {
			return err
		}
		current, exists := entries[ticket.ID]
		if !exists || current.Digest != ticket.Digest || current.Attempt != ticket.Attempt {
			return ErrConflict
		}
		if current.State == string(outcome) {
			return nil
		}
		if current.State != "pending" {
			return ErrConflict
		}
		current.State = string(outcome)
		return j.append(current)
	})
}

// Lookup only reads retained evidence. Absent never authorizes a new write,
// including when storage was lost or rolled back after an uncertain attempt.
func (j *Journal) Lookup(ctx context.Context, id string, payload []byte) (AdmissionState, error) {
	if !validID(id) || len(payload) == 0 || len(payload) > MaxPayloadBytes {
		return "", ErrConflict
	}
	hash := sha256.Sum256(payload)
	digest := hex.EncodeToString(hash[:])
	var state AdmissionState
	err := j.locked(ctx, func() error {
		entries, err := j.read()
		if err != nil {
			return err
		}
		record, exists := entries[id]
		if !exists {
			state = Absent
			return nil
		}
		if record.Digest != digest {
			return ErrConflict
		}
		switch record.State {
		case string(Delivered):
			state = Duplicate
		case string(NotSent):
			state = NotSentRecorded
		case "pending", string(Unconfirmed):
			state = Unknown
		default:
			return ErrCorrupt
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return state, nil
}

// Close is idempotent and waits for this handle's current filesystem operation.
func (j *Journal) Close() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.closed {
		return nil
	}
	j.closed = true
	return j.file.Close()
}

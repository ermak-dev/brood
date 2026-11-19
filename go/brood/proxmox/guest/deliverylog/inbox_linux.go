//go:build linux

package deliverylog

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const (
	inboxFile        = "inbox-v1.json"
	inboxStagingFile = ".inbox-v1-staging"
	MaxInboxEntries  = 256
	maxInboxPayload  = 128 << 10
	maxInboxBytes    = 8 << 20
	// Два идентификатора до 128 байт, их JSON-поля и самый длинный статус.
	inboxClaimHeadroom = 320 * MaxInboxEntries
)

var (
	ErrInboxMissing = errors.New("no retained inbox receipt")
	ErrInboxClaimed = errors.New("inbox instruction was already claimed; it cannot be replayed")
)

// InboxState describes processing after durable supervisor custody. It does
// not give permission to advance a graph or repeat a model effect.
type InboxState string

const (
	InboxQueued          InboxState = "queued"
	InboxClaimed         InboxState = "claimed"
	InboxSessionAccepted InboxState = "session_accepted"
	InboxFinished        InboxState = "finished"
	InboxUnknown         InboxState = "session_outcome_unknown"
)

// InboxReceipt identifies bytes durably accepted by the supervisor. It is not
// a native session admission, idle notice, or completion receipt.
type InboxReceipt struct {
	Scope                       Scope
	InboxID, DeliveryID, Digest string
}

// InboxStatus separates receiving custody from downstream processing.
type InboxStatus struct {
	Receipt InboxReceipt
	State   InboxState
}

// InboxTicket belongs to a single claim made under an authenticated launch
// and a server-reserved dispatch permit. Callers must verify both before use.
type InboxTicket struct {
	Receipt            InboxReceipt
	LaunchID, PermitID string
}

type inboxEntry struct {
	Payload  []byte     `json:"payload"`
	Digest   string     `json:"digest"`
	State    InboxState `json:"state"`
	LaunchID string     `json:"launchId,omitempty"`
	PermitID string     `json:"permitId,omitempty"`
}

type inboxSnapshot struct {
	Version     int                   `json:"version"`
	ID          string                `json:"id"`
	ActorUID    string                `json:"actorUid"`
	Incarnation int64                 `json:"incarnation"`
	Capacity    int                   `json:"capacity"`
	Entries     map[string]inboxEntry `json:"entries"`
}

// Inbox owns bounded receiving-side state in one actor incarnation's private
// namespace. It neither calls the Claude session nor uses the native journal.
type Inbox struct {
	lock     Journal
	dir      *os.File
	scope    Scope
	capacity int
	id       string
	dirSync  func() error
}

// OpenInboxDirectory opens an independent description of the private directory.
// The caller must establish a trusted namespace and sync each parent directory
// after creating its child, so the inbox remains reachable after a crash.
func OpenInboxDirectory(ctx context.Context, directory *os.File, scope Scope, capacity int) (*Inbox, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !validID(scope.ActorUID) || scope.Incarnation < 1 || capacity < 1 || capacity > MaxInboxEntries || directory == nil || !privateFile(directory, true) {
		return nil, ErrScope
	}
	// Flock the directory inode, not a replaceable lock file. Openat rather
	// than dup gives each handle its own open file description and lock.
	fd, err := unix.Openat(int(directory.Fd()), ".", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, ErrUnavailable
	}
	dir := os.NewFile(uintptr(fd), "delivery inbox")
	inbox := &Inbox{lock: Journal{file: dir}, dir: dir, scope: scope, capacity: capacity}
	if err := inbox.lock.locked(ctx, func() error {
		state, err := inbox.read()
		if errors.Is(err, os.ErrNotExist) {
			id, generateErr := inboxRandomID()
			if generateErr != nil {
				return generateErr
			}
			state = inboxSnapshot{Version: 1, ID: id, ActorUID: scope.ActorUID, Incarnation: scope.Incarnation, Capacity: capacity, Entries: map[string]inboxEntry{}}
			err = inbox.save(ctx, state, maxInboxBytes)
		}
		if err == nil {
			inbox.id = state.ID
		}
		return err
	}); err != nil {
		_ = inbox.Close()
		return nil, err
	}
	return inbox, nil
}

// Identifier pins later requests to the exact retained receiver.
func (i *Inbox) Identifier() string { return i.id }

// ActorScope is the immutable actor and incarnation bound to the inbox.
func (i *Inbox) ActorScope() Scope { return i.scope }

// Close releases the held directory and lock file, leaving history intact.
func (i *Inbox) Close() error {
	i.lock.mu.Lock()
	defer i.lock.mu.Unlock()
	if i.lock.closed {
		return nil
	}
	i.lock.closed = true
	return i.dir.Close()
}

func inboxRandomID() (string, error) {
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", ErrUnavailable
	}
	return hex.EncodeToString(nonce[:]), nil
}

func inboxDigest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func inboxHex(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == 32 && hex.EncodeToString(raw) == value
}

func (i *Inbox) syncDirectory() error {
	if i.dirSync != nil {
		if i.dirSync() != nil {
			return ErrUnavailable
		}
		return nil
	}
	if i.dir.Sync() != nil {
		return ErrUnavailable
	}
	return nil
}

func (i *Inbox) read() (inboxSnapshot, error) {
	var state inboxSnapshot
	// A previous process could have renamed the snapshot but failed to sync
	// its directory. Do not trust readable state until the rename is durable.
	if err := i.syncDirectory(); err != nil {
		return state, err
	}
	fd, err := unix.Openat(int(i.dir.Fd()), inboxFile, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return state, os.ErrNotExist
	}
	if err != nil {
		return state, ErrUnavailable
	}
	file := os.NewFile(uintptr(fd), inboxFile)
	defer file.Close()
	if !privateFile(file, false) {
		return state, ErrCorrupt
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxInboxBytes+1))
	if err != nil {
		return state, ErrUnavailable
	}
	if len(raw) > maxInboxBytes || json.Unmarshal(raw, &state) != nil {
		return inboxSnapshot{}, ErrCorrupt
	}
	// Our writer always emits canonical JSON. A different representation,
	// including aliases or duplicate keys, is not retained admission evidence.
	canonical, err := json.Marshal(state)
	if err != nil || !bytes.Equal(raw, canonical) || state.Version != 1 || !inboxHex(state.ID) || state.Entries == nil || len(state.Entries) > state.Capacity {
		return inboxSnapshot{}, ErrCorrupt
	}
	if state.ActorUID != i.scope.ActorUID || state.Incarnation != i.scope.Incarnation || state.Capacity != i.capacity || i.id != "" && state.ID != i.id {
		return inboxSnapshot{}, ErrScope
	}
	permits := make(map[string]bool)
	for id, entry := range state.Entries {
		if !validID(id) || len(entry.Payload) == 0 || len(entry.Payload) > maxInboxPayload || inboxDigest(entry.Payload) != entry.Digest {
			return inboxSnapshot{}, ErrCorrupt
		}
		switch entry.State {
		case InboxQueued:
			if entry.LaunchID != "" || entry.PermitID != "" {
				return inboxSnapshot{}, ErrCorrupt
			}
		case InboxClaimed, InboxSessionAccepted, InboxFinished, InboxUnknown:
			if !validID(entry.LaunchID) || !validID(entry.PermitID) || permits[entry.PermitID] {
				return inboxSnapshot{}, ErrCorrupt
			}
			permits[entry.PermitID] = true
		default:
			return inboxSnapshot{}, ErrCorrupt
		}
	}
	return state, nil
}

func (i *Inbox) save(ctx context.Context, state inboxSnapshot, limit int) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return ErrUnavailable
	}
	if len(raw) > limit {
		return ErrFull
	}
	dir := int(i.dir.Fd())
	file, err := openInboxStaging(dir)
	if err != nil {
		return err
	}
	defer unix.Unlinkat(dir, inboxStagingFile, 0)
	_, writeErr := file.Write(raw)
	syncErr := file.Sync()
	closeErr := file.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if unix.Renameat(dir, inboxStagingFile, dir, inboxFile) != nil {
		return ErrUnavailable
	}
	return i.syncDirectory()
}

// All writers hold the separate inbox flock. A previously abandoned staging
// file was never an admission receipt and cannot accumulate across crashes.
func openInboxStaging(dir int) (*os.File, error) {
	flags := unix.O_WRONLY | unix.O_CREAT | unix.O_EXCL | unix.O_NOFOLLOW | unix.O_CLOEXEC
	fd, err := unix.Openat(dir, inboxStagingFile, flags, 0600)
	if errors.Is(err, unix.EEXIST) {
		prior, openErr := unix.Openat(dir, inboxStagingFile, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
		if openErr != nil {
			return nil, ErrUnavailable
		}
		file := os.NewFile(uintptr(prior), inboxStagingFile)
		valid := privateFile(file, false)
		_ = file.Close()
		if !valid || unix.Unlinkat(dir, inboxStagingFile, 0) != nil {
			return nil, ErrUnavailable
		}
		fd, err = unix.Openat(dir, inboxStagingFile, flags, 0600)
	}
	if err != nil {
		return nil, ErrUnavailable
	}
	file := os.NewFile(uintptr(fd), inboxStagingFile)
	if file.Chmod(0600) != nil || !privateFile(file, false) {
		_ = file.Close()
		_ = unix.Unlinkat(dir, inboxStagingFile, 0)
		return nil, ErrUnavailable
	}
	return file, nil
}

func (i *Inbox) withSnapshot(ctx context.Context, id string, use func(inboxSnapshot) error) error {
	if id != i.id {
		return ErrScope
	}
	return i.lock.locked(ctx, func() error {
		state, err := i.read()
		if errors.Is(err, os.ErrNotExist) {
			return ErrUnavailable
		}
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		return use(state)
	})
}

func (i *Inbox) receipt(id, digest string) InboxReceipt {
	return InboxReceipt{Scope: i.scope, InboxID: i.id, DeliveryID: id, Digest: digest}
}

// Enqueue establishes custody of canonical bytes. An exact duplicate returns
// the same receipt without changing queue or downstream state.
func (i *Inbox) Enqueue(ctx context.Context, inboxID, id string, payload []byte) (InboxReceipt, error) {
	if !validID(id) || len(payload) == 0 || len(payload) > maxInboxPayload {
		return InboxReceipt{}, ErrConflict
	}
	payload = bytes.Clone(payload)
	digest := inboxDigest(payload)
	err := i.withSnapshot(ctx, inboxID, func(state inboxSnapshot) error {
		if entry, exists := state.Entries[id]; exists {
			if entry.Digest != digest || !bytes.Equal(entry.Payload, payload) {
				return ErrConflict
			}
			return nil
		}
		if len(state.Entries) >= state.Capacity {
			return ErrFull
		}
		state.Entries[id] = inboxEntry{Payload: payload, Digest: digest, State: InboxQueued}
		return i.save(ctx, state, maxInboxBytes-inboxClaimHeadroom)
	})
	if err != nil {
		return InboxReceipt{}, err
	}
	return i.receipt(id, digest), nil
}

// Lookup reads custody and downstream state without reserving another effect.
func (i *Inbox) Lookup(ctx context.Context, inboxID, id, digest string) (InboxStatus, error) {
	var status InboxStatus
	err := i.withSnapshot(ctx, inboxID, func(state inboxSnapshot) error {
		entry, exists := state.Entries[id]
		if !exists {
			return ErrInboxMissing
		}
		if entry.Digest != digest {
			return ErrConflict
		}
		status = InboxStatus{Receipt: i.receipt(id, digest), State: entry.State}
		return nil
	})
	return status, err
}

// Claim records one irrevocable handoff before releasing payload. No claim is
// repeated, even when its caller loses the response or presents a new permit.
func (i *Inbox) Claim(ctx context.Context, receipt InboxReceipt, launchID, permitID string) (InboxTicket, []byte, error) {
	if receipt != i.receipt(receipt.DeliveryID, receipt.Digest) || !validID(launchID) || !validID(permitID) {
		return InboxTicket{}, nil, ErrConflict
	}
	var payload []byte
	err := i.withSnapshot(ctx, receipt.InboxID, func(state inboxSnapshot) error {
		entry, exists := state.Entries[receipt.DeliveryID]
		if !exists || entry.Digest != receipt.Digest {
			return ErrConflict
		}
		if entry.State != InboxQueued {
			return ErrInboxClaimed
		}
		for _, retained := range state.Entries {
			if retained.PermitID == permitID {
				return ErrConflict
			}
		}
		entry.State, entry.LaunchID, entry.PermitID = InboxClaimed, launchID, permitID
		state.Entries[receipt.DeliveryID] = entry
		if err := i.save(ctx, state, maxInboxBytes); err != nil {
			return err
		}
		payload = entry.Payload
		return nil
	})
	if err != nil {
		return InboxTicket{}, nil, err
	}
	return InboxTicket{Receipt: receipt, LaunchID: launchID, PermitID: permitID}, payload, nil
}

// Report stores an observation for the exact claim. The ticket is correlation,
// not authentication: callers must verify the launch and the native evidence
// independently. Unknown and finished are terminal; a turn is not a Brood result.
func (i *Inbox) Report(ctx context.Context, ticket InboxTicket, next InboxState) error {
	receipt := ticket.Receipt
	if receipt != i.receipt(receipt.DeliveryID, receipt.Digest) || !validID(ticket.LaunchID) || !validID(ticket.PermitID) {
		return ErrConflict
	}
	if next != InboxSessionAccepted && next != InboxFinished && next != InboxUnknown {
		return ErrConflict
	}
	return i.withSnapshot(ctx, receipt.InboxID, func(state inboxSnapshot) error {
		entry, exists := state.Entries[receipt.DeliveryID]
		if !exists || entry.Digest != receipt.Digest || entry.LaunchID != ticket.LaunchID || entry.PermitID != ticket.PermitID {
			return ErrConflict
		}
		if entry.State == next {
			return nil
		}
		if !(entry.State == InboxClaimed && (next == InboxSessionAccepted || next == InboxUnknown) || entry.State == InboxSessionAccepted && next == InboxFinished) {
			return ErrConflict
		}
		entry.State = next
		state.Entries[receipt.DeliveryID] = entry
		return i.save(ctx, state, maxInboxBytes)
	})
}

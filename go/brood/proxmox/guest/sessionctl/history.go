//go:build linux

package sessionctl

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"brood.dev/go/brood/proxmox/relay"
)

const maxTranscriptPrefix = 8 * 1024 * 1024

type historyPage struct {
	Epoch     string            `json:"epoch"`
	SessionID string            `json:"session_id"`
	Cursor    string            `json:"cursor"`
	Records   []json.RawMessage `json:"records"`
	Reset     bool              `json:"reset"`
	HasMore   bool              `json:"has_more"`
	Partial   bool              `json:"partial"`
}
type historyCursor struct {
	Epoch    string `json:"epoch"`
	Identity string `json:"identity"`
	Device   uint64 `json:"device"`
	Inode    uint64 `json:"inode"`
	Offset   int64  `json:"offset"`
	Prefix   string `json:"prefix"`
	Skip     bool   `json:"skip"`
}

func (e *engine) history(ctx context.Context, cursor string, limit int) relay.SessionResponse {
	var previous historyCursor
	if cursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || decodeArgs(data, &previous) != nil || previous.Offset < 0 || previous.Offset > maxTranscriptPrefix || len(previous.Prefix) != 64 || len(previous.Identity) != 64 || len(previous.Epoch) != 64 {
			return failure("invalid history cursor")
		}
	}
	id, err := e.prove(ctx)
	if err != nil {
		return failure("no proven live Claude session")
	}
	if !safeID.MatchString(id.SessionID) {
		return failure("unsafe live session identity")
	}
	identityData, _ := json.Marshal(id)
	base := digest(identityData)
	path := filepath.Join(e.config, "projects", "-workspace-project", id.SessionID+".jsonl")
	f, err := openSafe(path, false)
	if os.IsNotExist(err) {
		after, proofErr := e.prove(ctx)
		if proofErr != nil || id != after {
			return failure("live session changed during history read")
		}
		c := historyCursor{Epoch: base, Identity: base, Prefix: digest(nil)}
		encoded, _ := json.Marshal(c)
		return success(historyPage{Epoch: base, SessionID: id.SessionID, Cursor: base64.RawURLEncoding.EncodeToString(encoded), Records: []json.RawMessage{}, Partial: true, Reset: cursor != "" && (previous.Identity != base || previous.Inode != 0)})
	}
	if err != nil {
		return failure("transcript unavailable or unsafe")
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return failure("transcript metadata unavailable")
	}
	stat := st.Sys().(*syscall.Stat_t)
	current := historyCursor{Identity: base, Device: uint64(stat.Dev), Inode: stat.Ino, Prefix: digest(nil)}
	current.Epoch = digest([]byte(base + ":" + string(encodedFileID(current))))
	reset := false
	if cursor != "" {
		compatible := previous.Identity == base && previous.Device == current.Device && previous.Inode == current.Inode && previous.Offset <= st.Size()
		if compatible {
			prefix, err := readAt(f, 0, previous.Offset)
			compatible = err == nil && digest(prefix) == previous.Prefix
		}
		if compatible {
			current.Offset = previous.Offset
			current.Skip = previous.Skip
			current.Epoch = previous.Epoch
		} else {
			reset = true
			current.Epoch = digest([]byte(current.Epoch + previous.Epoch + previous.Prefix))
		}
	}
	if current.Offset >= maxTranscriptPrefix {
		return failure("history cursor reached bounded transcript scan limit")
	}
	raw, err := readAt(f, current.Offset, min(64*1024, st.Size()-current.Offset))
	if err != nil {
		return failure("transcript read failed")
	}
	records := []json.RawMessage{}
	pos, used := 0, 0
	partial := false
	for pos < len(raw) && len(records) < limit {
		end := bytes.IndexByte(raw[pos:], '\n')
		complete := end >= 0
		stop := len(raw)
		if complete {
			stop = pos + end + 1
		}
		line := raw[pos:stop]
		if !complete && len(line) <= pageBudget && !current.Skip {
			partial = true
			break
		}
		var record []byte
		skip := !complete
		if current.Skip || len(line) > pageBudget {
			if len(line) > pageBudget {
				stop = pos + pageBudget
				line = raw[pos:stop]
				complete = false
				skip = true
			}
			record, _ = json.Marshal(map[string]any{"type": "brood_history_notice", "reason": "record_too_large", "offset": current.Offset + int64(pos), "skipped_bytes": len(line)})
		} else {
			var obj map[string]json.RawMessage
			if json.Unmarshal(line, &obj) != nil || obj == nil {
				record = []byte(`{"type":"brood_history_notice","reason":"invalid_record"}`)
			} else {
				record, _ = json.Marshal(obj)
			}
		}
		if len(record) > pageBudget {
			record, _ = json.Marshal(map[string]any{"type": "brood_history_notice", "reason": "encoded_record_too_large", "offset": current.Offset + int64(pos), "skipped_bytes": len(line)})
		}
		if used+len(record) > pageBudget {
			break
		}
		records = append(records, json.RawMessage(record))
		used += len(record)
		pos = stop
		current.Skip = skip
		partial = partial || skip
	}
	current.Offset += int64(pos)
	prefix, err := readAt(f, 0, current.Offset)
	if err != nil {
		return failure("transcript prefix unavailable")
	}
	if cursor != "" && !reset && digest(prefix[:previous.Offset]) != previous.Prefix {
		return failure("transcript changed during history read")
	}
	if len(prefix) < pos || !bytes.Equal(prefix[len(prefix)-pos:], raw[:pos]) {
		return failure("transcript changed during history read")
	}
	current.Prefix = digest(prefix)
	after, err := e.prove(ctx)
	if err != nil || id != after {
		return failure("live session changed during history read")
	}
	// Проверяем не только открытый fd: замена пути не выдаёт старую страницу.
	check, err := openSafe(path, false)
	if err != nil {
		return failure("transcript changed during history read")
	}
	defer check.Close()
	checked, err := check.Stat()
	if err != nil || !os.SameFile(st, checked) || checked.Size() < st.Size() {
		return failure("transcript changed during history read")
	}
	verify, err := readAt(check, 0, current.Offset)
	if err != nil || digest(verify) != current.Prefix {
		return failure("transcript changed during history read")
	}
	encoded, _ := json.Marshal(current)
	return success(historyPage{Epoch: current.Epoch, SessionID: id.SessionID, Cursor: base64.RawURLEncoding.EncodeToString(encoded), Records: records, Reset: reset, HasMore: current.Offset < st.Size() && !(partial && !current.Skip), Partial: partial})
}
func encodedFileID(c historyCursor) []byte {
	data, _ := json.Marshal([]uint64{c.Device, c.Inode})
	return data
}
func readAt(f *os.File, offset, length int64) ([]byte, error) {
	if offset < 0 || length < 0 || offset+length > maxTranscriptPrefix {
		return nil, errors.New("bounded transcript limit")
	}
	data := make([]byte, int(length))
	n, err := f.ReadAt(data, offset)
	if err == io.EOF && n == len(data) {
		err = nil
	}
	return data[:n], err
}

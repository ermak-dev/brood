//go:build linux

package guest

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
)

// EphemeralWorkspace retains only the server-assigned bootstrap commit. It does
// not carry a child payload, actor credentials, proof or effect permission.
type EphemeralWorkspace struct {
	Commit string `json:"commit"`
}

func validPinnedCommit(value string) bool {
	commit, err := hex.DecodeString(value)
	return err == nil && len(commit) == 20 && value == strings.ToLower(value)
}

func decodeEphemeralWorkspace(raw json.RawMessage) (*EphemeralWorkspace, bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	if first, err := d.Token(); err != nil || first != json.Delim('{') {
		return nil, false
	}
	if key, err := d.Token(); err != nil || key != "commit" {
		return nil, false
	}
	token, err := d.Token()
	commit, ok := token.(string)
	if err != nil || !ok || !validPinnedCommit(commit) {
		return nil, false
	}
	if end, err := d.Token(); err != nil || end != json.Delim('}') || d.Decode(new(any)) != io.EOF {
		return nil, false
	}
	return &EphemeralWorkspace{Commit: commit}, true
}

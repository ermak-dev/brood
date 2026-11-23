//go:build linux

package ccmsg

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

var sessionUUID = regexp.MustCompile(`^[a-fA-F0-9]{8}(-[a-fA-F0-9]{4}){3}-[a-fA-F0-9]{12}$`)

// TranscriptHasPeerMessage confirms only the exact peer message accepted by
// the selected session, when its separate callback did not arrive.
func TranscriptHasPeerMessage(sessionID, messageID string) bool {
	if os.Geteuid() != 1000 || !sessionUUID.MatchString(sessionID) || !sessionUUID.MatchString(messageID) {
		return false
	}
	f, info := openClaudeRecord("/workspace/home/.claude/projects/-workspace-project/" + sessionID + ".jsonl")
	if f == nil {
		return false
	}
	defer f.Close()
	const maxTail = 512 << 10
	start := max(int64(0), info.Size()-maxTail)
	data, err := io.ReadAll(io.NewSectionReader(f, start, info.Size()-start))
	if err != nil {
		return false
	}
	if start > 0 {
		_, data, _ = bytes.Cut(data, []byte{'\n'})
	}
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if !bytes.Contains(line, []byte(messageID)) {
			continue
		}
		var record struct {
			Type      string `json:"type"`
			SessionID string `json:"sessionId"`
			Origin    struct {
				Kind      string `json:"kind"`
				MessageID string `json:"msg_id"`
				SenderPID int    `json:"verifiedPeerPid"`
			} `json:"origin"`
		}
		if json.Unmarshal(line, &record) == nil && record.Type == "user" && record.SessionID == sessionID && record.Origin.Kind == "peer" && record.Origin.MessageID == messageID && record.Origin.SenderPID == os.Getpid() {
			return true
		}
	}
	return false
}

// ReadSessionActivity reads the structured state of the running child only.
func ReadSessionActivity(pid int) (string, string, time.Time) {
	if os.Geteuid() != 1000 || pid <= 0 {
		return "", "", time.Time{}
	}
	f, info := openClaudeRecord("/workspace/home/.claude/sessions/" + strconv.Itoa(pid) + ".json")
	if f == nil {
		return "", "", time.Time{}
	}
	defer f.Close()
	const limit = 128 << 10
	if info.Size() > limit {
		return "", "", time.Time{}
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	var s struct {
		PID       int     `json:"pid"`
		ID        string  `json:"sessionId"`
		CWD       string  `json:"cwd"`
		Status    string  `json:"status"`
		UpdatedAt float64 `json:"statusUpdatedAt"`
	}
	if err != nil || len(raw) > limit || json.Unmarshal(raw, &s) != nil || s.PID != pid || !sessionUUID.MatchString(s.ID) || s.CWD != "/workspace/project" || s.UpdatedAt <= 0 {
		return "", "", time.Time{}
	}
	return s.ID, s.Status, time.UnixMilli(int64(s.UpdatedAt))
}

func openClaudeRecord(path string) (*os.File, os.FileInfo) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil
	}
	f := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Uid != uint32(os.Geteuid()) {
		f.Close()
		return nil, nil
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil
	}
	return f, info
}

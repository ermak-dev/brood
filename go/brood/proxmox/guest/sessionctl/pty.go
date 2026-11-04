//go:build linux

package sessionctl

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"brood.dev/go/brood/proxmox/guest/ptysession"
	"brood.dev/go/brood/proxmox/relay"
)

type ptyCall func(context.Context, ptysession.Request) (ptysession.Reply, error)

func callPTY(ctx context.Context, req ptysession.Request) (ptysession.Reply, error) {
	return trustedPTYCall(ctx, ptysession.SocketPath, 0, 1000, "/", req)
}

// trustedPTYCall never contacts a user-replaceable endpoint. The trust anchor is
// private and configurable only for fixtures; production always checks from /.
func trustedPTYCall(ctx context.Context, path string, uid, gid uint32, anchor string, req ptysession.Request) (ptysession.Reply, error) {
	before, err := trustedPTYSocket(path, uid, gid, anchor)
	if err != nil {
		return ptysession.Reply{Outcome: "refused"}, err
	}
	reply, callErr := ptysession.Call(ctx, path, req)
	after, err := trustedPTYSocket(path, uid, gid, anchor)
	if err != nil || !os.SameFile(before, after) {
		return ptysession.Reply{}, errors.New("PTY endpoint changed; delivery is unconfirmed")
	}
	return reply, callErr
}

func trustedPTYSocket(path string, uid, gid uint32, anchor string) (os.FileInfo, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || !filepath.IsAbs(anchor) || filepath.Clean(anchor) != anchor {
		return nil, errors.New("unsafe PTY endpoint path")
	}
	parent := filepath.Dir(path)
	rel, err := filepath.Rel(anchor, parent)
	if err != nil || rel == ".." || (len(rel) > 3 && rel[:3] == "../") {
		return nil, errors.New("PTY endpoint outside trust anchor")
	}
	for dir := parent; ; dir = filepath.Dir(dir) {
		f, err := openSafe(dir, true)
		if err != nil {
			return nil, errors.New("PTY endpoint directory unavailable or unsafe")
		}
		st, err := f.Stat()
		f.Close()
		if err != nil {
			return nil, err
		}
		owner, ok := st.Sys().(*syscall.Stat_t)
		if !ok || owner.Uid != uid || st.Mode().Perm()&0022 != 0 {
			return nil, errors.New("PTY endpoint directory must be protected and root-owned")
		}
		if dir == anchor {
			break
		}
		if dir == "/" {
			return nil, errors.New("PTY endpoint trust anchor mismatch")
		}
	}
	st, err := os.Lstat(path)
	if err != nil {
		return nil, errors.New("PTY endpoint unavailable")
	}
	owner, ok := st.Sys().(*syscall.Stat_t)
	if !ok || st.Mode()&os.ModeSocket == 0 || owner.Uid != uid || owner.Gid != gid || st.Mode().Perm() != 0660 {
		return nil, errors.New("PTY endpoint must be a trusted root-owned socket")
	}
	return st, nil
}

func ptyFailure(reply ptysession.Reply, err error, mutation bool) relay.SessionResponse {
	if mutation && reply.Outcome != "refused" {
		return relay.SessionResponse{Outcome: "unknown", Text: "PTY delivery is unknown; do not retry automatically"}
	}
	if err != nil {
		return failure("trusted PTY control unavailable; nothing changed")
	}
	if reply.Error != "" {
		return failure(reply.Error)
	}
	return failure("PTY request refused; nothing changed")
}

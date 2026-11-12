//go:build linux

package main

import (
	"context"
	"encoding/json"

	"golang.org/x/sys/unix"

	"brood.dev/go/brood/proxmox/relay"
)

// releaseSessions отвечает на relay.GuestReleaseOperation (#133) из корня
// артефактов root, а остальные операции отдаёт supervisor-обработчику next
// без изменений. Ответ читает только раскладку root, поэтому ему не нужны ни
// UID 1000 helper, ни gate терминала: он ничего не меняет в CT.
func releaseSessions(root string, next func(context.Context, relay.SessionRequest) relay.SessionResponse) func(context.Context, relay.SessionRequest) relay.SessionResponse {
	return func(ctx context.Context, req relay.SessionRequest) relay.SessionResponse {
		if req.Operation == relay.GuestReleaseOperation {
			return reportRelease(root)
		}
		return next(ctx, req)
	}
}

// reportRelease называет набор, на который указывает bin в корне root (#133).
// Только чтение ссылки, без загрузки и хеширования: целостность набора
// bootstrap проверил при старте, а bin до следующего старта не меняется.
// Плоский каталог bin — bootstrap до #184, layout plain. Корень, которому
// installRelease не доверил бы (ссылка, чужой владелец, запись для других),
// отсутствие bin, посторонняя или кривая ссылка — отказ без догадки: сервер
// назовёт исходный набор unknown.
func reportRelease(root string) relay.SessionResponse {
	unreadable := relay.SessionResponse{Text: "guest release is not readable", Outcome: "failed"}
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return unreadable
	}
	defer unix.Close(fd)
	if !trusted(fd, unix.S_IFDIR) {
		return unreadable
	}
	answer := relay.GuestRelease{Layout: "release", Release: releaseLink(fd, binName)}
	if answer.Release == "" {
		var st unix.Stat_t
		if unix.Fstatat(fd, binName, &st, unix.AT_SYMLINK_NOFOLLOW) != nil || st.Mode&unix.S_IFMT != unix.S_IFDIR {
			return unreadable
		}
		answer = relay.GuestRelease{Layout: "plain"}
	}
	data, err := json.Marshal(answer)
	if err != nil {
		return unreadable
	}
	return relay.SessionResponse{OK: true, Text: string(data), Outcome: "complete"}
}

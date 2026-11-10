//go:build linux

package ptysession

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

func liveProcessGroup(stat []byte, group int) (bool, error) {
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return false, errors.New("invalid process stat")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 3 {
		return false, errors.New("incomplete process stat")
	}
	pgid, err := strconv.Atoi(fields[2])
	if err != nil {
		return false, errors.New("invalid process group")
	}
	return pgid == group && fields[0] != "Z" && fields[0] != "X" && fields[0] != "x", nil
}

// Лидер остаётся waitable с WNOWAIT на всём протяжении проверки:
// его PID не может стать идентификатором чужой process group.
func finishPinnedGroup(pid int) error {
	return finishGroupWith(pid, os.ReadFile)
}

// finishGroupWith получает чтение stat аргументом: тест забирает процесс
// между open и read детерминированно, а не ждёт гонки под нагрузкой.
func finishGroupWith(pid int, readStat func(path string) ([]byte, error)) error {
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil && err != unix.ESRCH {
		return fmt.Errorf("PTY process group signal failed: %w", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return errors.New("PTY process group enumeration failed")
		}
		live := false
		for _, entry := range entries {
			if _, err := strconv.Atoi(entry.Name()); err != nil {
				continue
			}
			stat, err := readStat("/proc/" + entry.Name() + "/stat")
			// Обход видит каждый процесс пространства PID, и любой из них может
			// завершиться посреди обхода. Забранный до open даёт ENOENT, а
			// забранный родителем между open и read: ESRCH, ядро не находит задачу
			// открытого файла. Такой процесс уже не участник группы; прочие отказы
			// оставляют членство неподтверждённым.
			if os.IsNotExist(err) || errors.Is(err, unix.ESRCH) {
				continue
			}
			if err != nil {
				return errors.New("PTY process group membership is unconfirmed")
			}
			member, err := liveProcessGroup(stat, pid)
			if err != nil {
				return err
			}
			live = live || member
		}
		if !live {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.New("PTY process group exit is unconfirmed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

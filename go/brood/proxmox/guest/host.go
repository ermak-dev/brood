//go:build linux

package guest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"syscall"
	"time"
)

// Стоковый Debian 12 не несёт ничего для brood: всё, что раньше клал образ,
// агент готовит сам при каждом старте. Пути и владельцы — те же, что создавал
// прежний Dockerfile, их проверяют delivery_config.go и delivery_journal.go.
const (
	hostConfigDirectory   = "/etc/brood"
	hostStateDirectory    = "/var/lib/brood"
	hostDeliveryDirectory = hostStateDirectory + "/delivery"
	hostDeliveryPointer   = hostConfigDirectory + "/delivery-state-dir"
	actorAccount          = "actor"
	actorID               = "1000"
	ttydPath              = "/usr/bin/ttyd"
)

// hostPackages: файл, без которого runtime не работает, и пакет Debian с ним.
// git зовут клон, checkout и проекция секретов; корневые сертификаты нужны
// машинному маршруту без BROOD_GUEST_CA_PEM_B64 и Git по HTTPS.
var hostPackages = []struct{ path, name string }{
	{"/usr/bin/git", "git"},
	{"/etc/ssl/certs/ca-certificates.crt", "ca-certificates"},
}

// PrepareHost makes a stock Debian 12 system container fit for the runtime.
// It is idempotent and creates only what is missing: the actor account
// (UID/GID 1000), /workspace with the actor home and project, /etc/brood, the
// delivery state directory with its pointer file, and the Debian packages the
// runtime executes. Only guest root may call it, before the actor starts.
// The returned notices are static texts about optional parts left absent.
func PrepareHost(ctx context.Context) ([]string, error) {
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		return nil, errors.New("host preparation requires guest root")
	}
	if err := prepareActorAccount(ctx); err != nil {
		return nil, err
	}
	if err := prepareWorkspace("/workspace", 1000, 1000); err != nil {
		return nil, err
	}
	if err := prepareDeliveryState(); err != nil {
		return nil, err
	}
	if err := installHostPackages(ctx); err != nil {
		return nil, err
	}
	var notices []string
	// В Debian нет пакета ttyd, а качать бинарь из сети агент не должен: его
	// кладёт установщик. Без него актор работает, но без терминала в браузере.
	if _, err := os.Stat(ttydPath); os.IsNotExist(err) {
		notices = append(notices, ttydPath+" is missing and Debian has no ttyd package: the browser terminal is unavailable")
	}
	return notices, nil
}

// prepareActorAccount заводит группу и пользователя актора, только когда их
// нет. UID 1000 принадлежит актору целиком (его процессы убивает sweep),
// поэтому чужая запись с этим номером — отказ, а не повод её переиспользовать.
func prepareActorAccount(ctx context.Context) error {
	group, err := user.LookupGroupId(actorID)
	if _, missing := err.(user.UnknownGroupIdError); missing {
		if err := runHostTool(ctx, time.Minute, "/usr/sbin/groupadd", "--gid", actorID, actorAccount); err != nil {
			return errors.New("cannot create the actor group: " + err.Error())
		}
	} else if err != nil {
		return errors.New("cannot inspect the group database")
	} else if group.Name != actorAccount {
		return errors.New("GID 1000 belongs to another group")
	}
	account, err := user.LookupId(actorID)
	if _, missing := err.(user.UnknownUserIdError); missing {
		if err := runHostTool(ctx, time.Minute, "/usr/sbin/useradd", "--uid", actorID, "--gid", actorID, "--home-dir", "/workspace/home", "--no-create-home", "--shell", "/bin/bash", actorAccount); err != nil {
			return errors.New("cannot create the actor user: " + err.Error())
		}
		return nil
	}
	if err != nil {
		return errors.New("cannot inspect the user database")
	}
	if account.Username != actorAccount || account.Gid != actorID {
		return errors.New("UID 1000 belongs to another user")
	}
	return nil
}

// prepareDeliveryState создаёт каталоги и файл-указатель журнала доставки.
// Существующий указатель не трогаем: его читает и проверяет
// ReadSupervisorDeliveryDirectory, история доставки не переезжает молча.
func prepareDeliveryState() error {
	for _, directory := range []struct {
		path string
		mode os.FileMode
	}{{hostConfigDirectory, 0700}, {hostStateDirectory, 0755}, {hostDeliveryDirectory, 0700}} {
		if err := os.Mkdir(directory.path, directory.mode); err != nil && !os.IsExist(err) {
			return errors.New("cannot create " + directory.path)
		}
		info, err := os.Lstat(directory.path)
		if err != nil || !info.IsDir() {
			return errors.New(directory.path + " must be a real directory")
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || st.Uid != 0 {
			return errors.New(directory.path + " must be owned by root")
		}
		if err := os.Chmod(directory.path, directory.mode); err != nil {
			return errors.New("cannot secure " + directory.path)
		}
	}
	if _, err := os.Lstat(hostDeliveryPointer); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return errors.New("cannot inspect " + hostDeliveryPointer)
	}
	// Запись через временный файл: оборванный старт не оставит пустой
	// указатель, который runtime отвергнет навсегда.
	temporary := hostDeliveryPointer + ".tmp"
	_ = os.Remove(temporary)
	err := os.WriteFile(temporary, []byte(hostDeliveryDirectory+"\n"), 0600)
	if err == nil {
		err = os.Chmod(temporary, 0600)
	}
	if err == nil {
		err = os.Rename(temporary, hostDeliveryPointer)
	}
	if err != nil {
		_ = os.Remove(temporary)
		return errors.New("cannot write " + hostDeliveryPointer)
	}
	return nil
}

// installHostPackages ставит недостающие пакеты Debian и не зовёт apt, когда
// все файлы на месте.
func installHostPackages(ctx context.Context) error {
	var missing []string
	for _, item := range hostPackages {
		if _, err := os.Stat(item.path); os.IsNotExist(err) {
			missing = append(missing, item.name)
		} else if err != nil {
			return errors.New("cannot inspect " + item.path)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	// Свежий шаблон приходит без списков пакетов; замок dpkg может держать
	// apt-daily первого старта.
	if err := runHostTool(ctx, 5*time.Minute, "/usr/bin/apt-get", "-o", "DPkg::Lock::Timeout=120", "update"); err != nil {
		return errors.New("cannot refresh the Debian package lists: " + err.Error())
	}
	args := append([]string{"-o", "DPkg::Lock::Timeout=120", "install", "-y", "--no-install-recommends"}, missing...)
	if err := runHostTool(ctx, 10*time.Minute, "/usr/bin/apt-get", args...); err != nil {
		return errors.New("cannot install Debian packages " + strings.Join(missing, " ") + ": " + err.Error())
	}
	for _, item := range hostPackages {
		if _, err := os.Stat(item.path); err != nil {
			return errors.New(item.path + " is still missing after installing " + item.name)
		}
	}
	return nil
}

// runHostTool запускает доверенный инструмент дистрибутива с фиксированным
// окружением, без stdin и без секретов агента; причина отказа — последняя
// строка его диагностики.
func runHostTool(parent context.Context, timeout time.Duration, name string, args ...string) error {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "LANG=C", "DEBIAN_FRONTEND=noninteractive"}
	cmd.Dir = "/"
	cmd.WaitDelay = 5 * time.Second
	stderr := &tailBuffer{limit: 4 << 10}
	cmd.Stderr = stderr
	err := cmd.Run()
	if ctx.Err() != nil {
		return errors.New("timed out or canceled")
	}
	if err != nil {
		if strings.TrimSpace(string(stderr.Bytes())) != "" {
			return errors.New(gitReason(stderr.Bytes()))
		}
		return errors.New(safeExecError(err))
	}
	return nil
}

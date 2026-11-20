package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"brood.dev/go/brood/proxmox"
	"brood.dev/go/brood/proxmox/sshboot"
	"brood.dev/go/brood/store"
)

// Агент brood в контейнере воркера. Контейнер — системный Debian из
// стокового шаблона или клон образа проекта; сервер заходит в него по SSH
// ключом установки, который PVE положил при создании, ставит бинарник
// агента, его настройки и systemd-юнит и запускает сервис. Дальше агент сам
// подключается к relay и сам обновляет свой runtime с сервера.

const (
	agentBinaryPath = "/usr/local/bin/brood-guest"
	agentEnvPath    = "/etc/brood/agent.env"
	agentUnitPath   = "/etc/systemd/system/brood-agent.service"
	// ttyd нет в Debian: контейнер сам скачивает статический бинарник
	// закреплённого выпуска и сверяет sha256 из SHA256SUMS апстрима.
	agentTTYDURL    = "https://github.com/tsl0922/ttyd/releases/download/1.7.7/ttyd.x86_64"
	agentTTYDSHA256 = "8a217c968aba172e0dbf3f34447218dc015bc4d5e59bf51db2f2cd12b7be4f55"
)

const agentUnit = `[Unit]
Description=brood agent
Wants=network-online.target
After=network-online.target
StartLimitIntervalSec=0

[Service]
Type=simple
User=root
WorkingDirectory=/
EnvironmentFile=/etc/brood/agent.env
ExecStart=/usr/local/bin/brood-guest
Restart=always
RestartSec=5
KillMode=mixed
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
`

// errAgentWaiting — контейнер ещё не готов принять агента: адреса нет или
// sshd не отвечает. Следующий проход повторит.
var errAgentWaiting = errors.New("container is not reachable over SSH yet")

// loadAgentIdentity читает или создаёт SSH-ключ установки рядом с базой.
// Ключ переживает пересоздание базы: образы, собранные с ним, остаются
// доступны серверу. База в памяти ключа не имеет.
func loadAgentIdentity(dbPath string) (*sshboot.Identity, error) {
	if dbPath == "" || dbPath == ":memory:" {
		return nil, nil
	}
	absolute, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, err
	}
	return sshboot.LoadOrCreateIdentity(filepath.Join(filepath.Dir(absolute), "ssh_identity"))
}

// agentEnv — настройки агента воркера a: адрес и токен relay его
// инкарнации. Токен действует только для этого UID и поколения.
func (m *managedProvider) agentEnv(a store.ManagedActor) map[string]string {
	origin := strings.TrimSuffix(m.registry.publicOrigin, "/")
	env := map[string]string{
		"BROOD_GUEST_ACTOR_UID":      a.UID,
		"BROOD_GUEST_GENERATION":     managedDecimal(a.Incarnation),
		"BROOD_GUEST_RELAY_URL":      "wss" + strings.TrimPrefix(origin, "https") + "/api/proxmox/guest/" + a.UID + "/" + managedDecimal(a.Incarnation),
		"BROOD_GUEST_RELAY_TOKEN":    guestToken(m.registry.relaySecret, a.UID, a.Incarnation),
		"BROOD_GUEST_RELAY_HOST_KEY": strings.TrimSpace(m.hub.HostKey()),
	}
	if m.publicCA != "" {
		env["BROOD_GUEST_CA_PEM_B64"] = base64.StdEncoding.EncodeToString([]byte(m.publicCA))
	}
	return env
}

// agentEnvFile кодирует настройки для systemd EnvironmentFile. Значения не
// содержат кавычек, обратной косой черты и переводов строк; иное — отказ.
func agentEnvFile(env map[string]string) ([]byte, error) {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out bytes.Buffer
	for _, key := range keys {
		if strings.ContainsAny(env[key], "\"\\\r\n\x00$`") {
			return nil, fmt.Errorf("agent setting %s cannot be written to the environment file", key)
		}
		fmt.Fprintf(&out, "%s=\"%s\"\n", key, env[key])
	}
	return out.Bytes(), nil
}

// installAgent ставит и запускает агента в работающем контейнере воркера a.
// Повтор безопасен: файлы пишутся атомарно, сервис перезапускается. Успех
// запоминается на операцию, чтобы проходы reconcile не перезапускали
// агента, пока он подключается.
func (m *managedProvider) installAgent(ctx context.Context, client proxmoxAPI, a store.ManagedActor) error {
	if done, ok := m.agentInstalled.Load(a.UID); ok && done == a.OperationID {
		return nil
	}
	if m.identity == nil {
		return errors.New("installation SSH identity is unavailable")
	}
	binary, err := os.ReadFile(filepath.Join(m.server.artifactsDir, "brood-guest"))
	if err != nil || m.server.artifactsDir == "" {
		return errors.New("agent binary is not published; BROOD_ARTIFACTS_DIR must hold brood-guest")
	}
	addresses, err := client.Addresses(ctx, a.CTID)
	if err != nil || len(addresses) == 0 {
		return errAgentWaiting
	}
	target := sshboot.Target{Addr: netip.AddrPortFrom(addresses[0], 22)}
	pinKey := a.UID + "/" + managedDecimal(a.Incarnation)
	if key, ok := m.agentHostKeys.Load(pinKey); ok {
		target.HostKey = key.(string)
	}
	session, err := sshboot.Dial(ctx, m.identity, target)
	if err != nil {
		if errors.Is(err, sshboot.ErrHostKey) {
			return errors.New("container SSH host key changed since first contact")
		}
		return errAgentWaiting
	}
	defer session.Close()
	m.agentHostKeys.Store(pinKey, session.HostKey())
	envFile, err := agentEnvFile(m.agentEnv(a))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(binary)
	// Бинарник агента передаётся, только если в контейнере другой: клон
	// образа проекта обычно уже несёт нынешний.
	check := "printf '%s  %s\\n' " + sshboot.Quote(hex.EncodeToString(sum[:])) + " " + sshboot.Quote(agentBinaryPath) + " | sha256sum --check --status"
	if err := session.Run(ctx, check, nil, nil); err != nil {
		var exit *sshboot.ExitError
		if !errors.As(err, &exit) {
			return errAgentWaiting
		}
		if err := session.WriteFile(ctx, agentBinaryPath, 0o755, binary); err != nil {
			return errors.New("agent binary transfer failed")
		}
	}
	steps := []struct {
		what, command string
		file          string
		mode          os.FileMode
		data          []byte
	}{
		{what: "agent settings directory", command: "install -d -m 0700 /etc/brood"},
		{what: "agent settings", file: agentEnvPath, mode: 0o600, data: envFile},
		{what: "agent service unit", file: agentUnitPath, mode: 0o644, data: []byte(agentUnit)},
		{what: "terminal server", command: agentTTYDCommand},
		{what: "agent service start", command: "systemctl daemon-reload && systemctl enable brood-agent.service && systemctl restart brood-agent.service"},
	}
	for _, step := range steps {
		if step.file != "" {
			err = session.WriteFile(ctx, step.file, step.mode, step.data)
		} else {
			err = session.Run(ctx, step.command, nil, nil)
		}
		if err != nil {
			if ctx.Err() != nil {
				return errAgentWaiting
			}
			return fmt.Errorf("agent installation failed at: %s", step.what)
		}
	}
	m.agentInstalled.Store(a.UID, a.OperationID)
	return nil
}

// agentTTYDCommand ставит ttyd, если его нет или он другой: скачивание
// закреплённого выпуска, сверка sha256, атомарная замена.
var agentTTYDCommand = strings.Join([]string{
	"set -e",
	"want=" + sshboot.Quote(agentTTYDSHA256),
	`if printf '%s  /usr/bin/ttyd\n' "$want" | sha256sum --check --status 2>/dev/null; then exit 0; fi`,
	"export DEBIAN_FRONTEND=noninteractive",
	"command -v curl >/dev/null || { apt-get update -qq && apt-get install -y -qq --no-install-recommends curl ca-certificates >/dev/null; }",
	"tmp=$(mktemp /usr/bin/.ttyd.XXXXXX)",
	"trap 'rm -f \"$tmp\"' EXIT",
	"curl -fsSL --proto '=https' --max-time 120 -o \"$tmp\" " + sshboot.Quote(agentTTYDURL),
	`printf '%s  %s\n' "$want" "$tmp" | sha256sum --check --status`,
	"chmod 0755 \"$tmp\"",
	"mv -f \"$tmp\" /usr/bin/ttyd",
}, "\n")

// agentSpec — контейнер воркера a: системный Debian с ключом установки и
// nesting для systemd. Настроек агента в конфигурации PVE нет.
func (m *managedProvider) agentSpec(a store.ManagedActor) proxmox.CTSpec {
	spec := proxmox.CTSpec{VMID: a.CTID, Hostname: "brood-" + a.UID, Template: a.Template.URI, Owner: managedOwned(a).Owner, Cores: int(a.Resources.CPUMillicores / 1000), MemoryMiB: int(a.Resources.MemoryBytes / mib), DiskGiB: int(a.Resources.DiskBytes / gib), Nesting: true}
	if m.identity != nil {
		spec.SSHPublicKeys = m.identity.AuthorizedKey()
	}
	return spec
}

//go:build linux

package guest

// Сессия шага роя (#224). Подтверждение workspace утверждает дерево в окне
// тишины; после подписи root перезапускает Claude, и без политики вместе с ним
// стартовал бы код, сохранённый актором в home: hooks и statusLine настроек
// Claude, MCP-серверы ~/.claude.json и .mcp.json, env настроек, ~/.gitconfig
// (core.fsmonitor исполняется при первом git status) и rc-файлы bash, которые
// Claude читает при снимке shell. Сессия, начатая подтверждением, запускается
// в режиме шага и держит его до перезапуска вне шага: restart, recycle или
// новый старт CT. Свободная сессия запускается как до #224.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"brood.dev/go/brood/gitidentity"
)

// Файлы режима шага в /run/brood лежат рядом с проекцией origin и по тому же
// контракту, root:1000 0440 в каталоге root:1000 0750: UID 1000 читает их, но
// не создаёт, не подменяет и не удаляет. /run выбран, а не home, потому что
// режим — решение root, а не данные актора; root пишет его заново на каждом
// подтверждении и снимает при старте runtime.
const (
	// stepSessionFile — отметка режима шага; её читает app wrapper.
	stepSessionFile = "step-session.json"
	// stepGitConfigFile — вся глобальная конфигурация Git сессии шага
	// (GIT_CONFIG_GLOBAL): ~/.gitconfig и ~/.config/git/config не читаются.
	stepGitConfigFile = "step-gitconfig"
	// stepManagedFile — drop-in управляемых настроек Claude Code в
	// managed-settings.d: базовый managed-settings.json остаётся нетронутым.
	stepManagedFile = "brood-swarm-step.json"
)

var stepSessionMarker = []byte(`{"mode":"step"}` + "\n")

// stepManagedSettings — управляемые настройки сессии шага. allowManagedHooksOnly
// не даёт исполнить hooks настроек, plugins и frontmatter агентов, а statusLine,
// subagentStatusLine и fileSuggestion берёт только из управляемых настроек
// (settings-reference, «What runs under allowManagedHooksOnly»). Остальные
// ключи свободная сессия получает в ~/.claude/settings.json, который сессия
// шага не читает: согласие на bypass и автономный профиль #164.
// crossSessionInbound и feedbackDrafts Claude Code 2.1.310 берёт только из
// управляемых настроек, флагов и настроек пользователя, проект может лишь
// ужесточить первый. Drop-in живёт только в режиме шага, на свободную сессию
// не действует.
var stepManagedSettings = stepManagedDocument()

func stepManagedDocument() []byte {
	settings := map[string]json.RawMessage{"allowManagedHooksOnly": jsonTrue}
	autonomousSettings(settings)
	data, _ := json.Marshal(settings)
	return append(data, '\n')
}

// actorShellStartupFiles — файлы, которые bash читает при входе (-l), в
// интерактивном режиме и при exit login shell. Claude снимает окружение shell
// командой `$SHELL -c -l` с явным `source ~/.bashrc`, поэтому смены SHELL или
// BASH_ENV мало: файлы приводятся к образу. В образе их нет — актор создан с
// --no-create-home, home пуст, — и bash читает только /etc/profile и
// /etc/bash.bashrc образа; ~/.bash_aliases без ~/.bashrc не читается никем.
var actorShellStartupFiles = []string{".bash_profile", ".bash_login", ".profile", ".bashrc", ".bash_logout"}

// stepPolicy ставит и снимает режим шага. Root и RootGID — владелец файлов
// политики, ActorGID — группа, читающая файлы в Run, ActorUID — владелец Home.
// Тесты подставляют каталоги и владельцев; production — systemStepPolicy.
type stepPolicy struct {
	Run, Managed, Home                string
	Root, RootGID, ActorGID, ActorUID int
	Now                               func() time.Time
}

func systemStepPolicy() stepPolicy {
	return stepPolicy{Run: projectSecretDirectory, Managed: "/etc/claude-code", Home: "/workspace/home", Root: 0, RootGID: 0, ActorGID: 1000, ActorUID: 1000, Now: time.Now}
}

// enter ставит режим шага для следующей сессии. Вызывается только в окне
// тишины (Runtime.Quiesce): процессов UID 1000 нет, и home не меняется под
// руками root. Отметка пишется последней: без неё wrapper запускает свободную
// сессию, поэтому любой отказ оставляет согласованное состояние.
func (p stepPolicy) enter(identity gitidentity.Identity) error {
	if err := p.place(identity); err != nil {
		if cleanup := p.leave(); cleanup != nil {
			return fmt.Errorf("%w; cleanup failed: %w", err, cleanup)
		}
		return err
	}
	return nil
}

func (p stepPolicy) place(identity gitidentity.Identity) error {
	if !identity.IsZero() && identity.Validate() != nil {
		return errors.New("actor Git identity is invalid")
	}
	if err := p.writeRun(stepGitConfigFile, stepGitConfig(identity)); err != nil {
		return errors.New("step Git configuration cannot be published")
	}
	if err := p.writeManaged(); err != nil {
		return err
	}
	if err := p.asideShellStartup(); err != nil {
		return err
	}
	if err := p.writeRun(stepSessionFile, stepSessionMarker); err != nil {
		return errors.New("step session marker cannot be published")
	}
	return nil
}

// leave возвращает следующую сессию в свободный режим. Отметка снимается
// первой: если дальше что-то не удалится, свободная сессия лишь строже. Не
// снялась — остальное не трогается: wrapper запустит сессию шага по одной
// отметке, и она не должна пережить drop-in с allowManagedHooksOnly.
// Перенесённые rc-файлы не возвращаются: копии рядом с отметкой времени.
func (p stepPolicy) leave() error {
	if err := p.removeRun(stepSessionFile); err != nil {
		return errors.New("step session marker cannot be removed, so the rest of the step policy stays in place")
	}
	var reasons []string
	if err := p.removeManaged(); err != nil {
		reasons = append(reasons, "Claude managed settings of the step cannot be removed: "+err.Error())
	}
	if err := p.removeRun(stepGitConfigFile); err != nil {
		reasons = append(reasons, "step Git configuration cannot be removed")
	}
	if len(reasons) > 0 {
		return errors.New(strings.Join(reasons, "; "))
	}
	return nil
}

func (p stepPolicy) runDirectory() (int, error) {
	dir, err := openSecretDirectory(p.Run, false)
	if err != nil {
		return -1, err
	}
	if err := secureSecretDirectory(dir, p.Root, false); err != nil {
		unix.Close(dir)
		return -1, err
	}
	return dir, nil
}

func (p stepPolicy) writeRun(name string, data []byte) error {
	dir, err := p.runDirectory()
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	return replaceSecretFile(dir, name, data, 0440, p.Root, p.ActorGID, false)
}

func (p stepPolicy) removeRun(name string) error {
	dir, err := p.runDirectory()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	if err := unix.Unlinkat(dir, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Fsync(dir)
}

// managedDirectory открывает managed-settings.d без перехода по symlink.
// Каталоги и их родитель принадлежат root и не пишутся группой и прочими:
// иначе UID 1000 мог бы подменить политику. Созданные или найденные каталоги
// получают 0755: Claude читает политику как other, нечитаемый каталог
// управляемых настроек — отказ запуска. Отсутствие каталога остаётся
// os.ErrNotExist (снимать нечего), остальные отказы называют причину.
func (p stepPolicy) managedDirectory(create bool) (int, error) {
	fd, err := openSecretDirectory(filepath.Dir(p.Managed), false)
	if err != nil {
		return -1, p.managedFailure(err)
	}
	if err := secureSecretDirectory(fd, p.Root, false); err != nil {
		unix.Close(fd)
		return -1, p.managedFailure(err)
	}
	for _, name := range []string{filepath.Base(p.Managed), "managed-settings.d"} {
		if create {
			if err := unix.Mkdirat(fd, name, 0755); err != nil && !errors.Is(err, unix.EEXIST) {
				unix.Close(fd)
				return -1, p.managedFailure(err)
			}
		}
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return -1, p.managedFailure(err)
		}
		fd = next
		if err := secureSecretDirectory(fd, p.Root, false); err != nil {
			unix.Close(fd)
			return -1, p.managedFailure(err)
		}
		if create && unix.Fchmod(fd, 0755) != nil {
			unix.Close(fd)
			return -1, secretFailure()
		}
	}
	return fd, nil
}

// managedFailure — причина отказа каталога управляемых настроек для
// оператора, который чинит образ. Остаться открытым он не может: кто пишет
// этот каталог, тот задаёт управляемые hooks и env, которые сессия шага
// сохраняет, поэтому старт runtime отказывает так же, как на /workspace,
// который пишут другие, а не ждёт первого подтверждения.
func (p stepPolicy) managedFailure(err error) error {
	if errors.Is(err, os.ErrNotExist) {
		return err
	}
	return fmt.Errorf("Claude Code managed settings directory %s or its managed-settings.d is a link, is not owned by root or is writable by group or others, so the actor could replace the managed settings; make them root directories with mode 0755", p.Managed)
}

func (p stepPolicy) writeManaged() error {
	dir, err := p.managedDirectory(true)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("Claude Code managed settings directory %s cannot be created", p.Managed)
	}
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	// Временное имя начинается с точки: Claude такие drop-in не читает.
	if replaceSecretFile(dir, stepManagedFile, stepManagedSettings, 0644, p.Root, p.RootGID, false) != nil {
		return errors.New("Claude managed settings of the step cannot be published")
	}
	return nil
}

func (p stepPolicy) removeManaged() error {
	dir, err := p.managedDirectory(false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(dir)
	if err := unix.Unlinkat(dir, stepManagedFile, 0); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return unix.Fsync(dir)
}

// asideShellStartup переименовывает rc-файлы актора в копии рядом с отметкой
// времени: rename в том же каталоге не читает и не переписывает содержимое,
// не идёт по symlink и не теряет прежнюю копию (RENAME_NOREPLACE).
func (p stepPolicy) asideShellStartup() error {
	home, err := openSecretDirectory(p.Home, false)
	if err != nil {
		return errors.New("actor home is unavailable or unsafe")
	}
	defer unix.Close(home)
	var st unix.Stat_t
	if unix.Fstat(home, &st) != nil || st.Uid != uint32(p.ActorUID) {
		return errors.New("actor home is not owned by the actor")
	}
	if name, err := putAside(home, actorShellStartupFiles, asideStamp(p.Now())); err != nil {
		if name == "" {
			return errors.New("actor home cannot be synced")
		}
		return fmt.Errorf("actor shell startup file %s cannot be put aside", name)
	}
	return nil
}

// putAside переносит существующие имена каталога dir в копии рядом
// (moveAside) и синхронизирует каталог. Отказ называет имя, на котором
// перенос остановился; пустое имя — отказ синхронизации.
func putAside(dir int, names []string, stamp string) (string, error) {
	moved := false
	for _, name := range names {
		var entry unix.Stat_t
		err := unix.Fstatat(dir, name, &entry, unix.AT_SYMLINK_NOFOLLOW)
		if errors.Is(err, unix.ENOENT) {
			continue
		}
		if err == nil {
			err = moveAside(dir, name, stamp)
		}
		if err != nil {
			return name, err
		}
		moved = true
	}
	if moved {
		if err := unix.Fsync(dir); err != nil {
			return "", err
		}
	}
	return "", nil
}

func asideStamp(now time.Time) string { return now.UTC().Format("20060102T150405Z") }

// asideName — имя копии: прежние копии того же момента получают номер.
func asideName(name, stamp string, attempt int) string {
	if attempt == 0 {
		return name + ".brood-" + stamp
	}
	return name + ".brood-" + stamp + "." + strconv.Itoa(attempt)
}

func moveAside(dir int, name, stamp string) error {
	for attempt := 0; attempt < 100; attempt++ {
		err := unix.Renameat2(dir, name, dir, asideName(name, stamp, attempt), unix.RENAME_NOREPLACE)
		if !errors.Is(err, unix.EEXIST) {
			return err
		}
	}
	return unix.EEXIST
}

// stepGitConfig — вся глобальная конфигурация Git сессии шага: идентичность
// установки (#249) и ничего, что исполняет код (fsmonitor, hooksPath,
// фильтры, include). Без идентичности файл пуст. Значения в кавычках: имя
// может содержать #, ; и кавычки, а управляющих символов в нём нет.
func stepGitConfig(identity gitidentity.Identity) []byte {
	if identity.IsZero() {
		return nil
	}
	quote := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return []byte("[user]\n\tname = \"" + quote.Replace(identity.Name) + "\"\n\temail = \"" + quote.Replace(identity.Email) + "\"\n")
}

func stepFailure() error { return errors.New("step session policy unavailable or unsafe") }

// readStepSession сообщает app wrapper режим сессии. Отсутствие отметки —
// свободная сессия; отметка не того владельца, режима или содержимого —
// отказ: wrapper не запускает Claude, не зная режима.
func readStepSession(run string, uid, gid int) (bool, error) {
	dir, err := openSecretDirectory(run, false)
	if err != nil {
		return false, stepFailure()
	}
	defer unix.Close(dir)
	if secureSecretDirectory(dir, uid, false) != nil {
		return false, stepFailure()
	}
	data, st, err := readSecretFile(dir, stepSessionFile, len(stepSessionMarker), uid, 0440)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || st.Gid != uint32(gid) || !bytes.Equal(data, stepSessionMarker) {
		return false, stepFailure()
	}
	return true, nil
}

// stepAppArgs — запуск Claude в сессии шага. --setting-sources project не
// читает ~/.claude/settings.json и .claude/settings.local.json: их env
// (PATH, GIT_CONFIG_*, LD_PRELOAD), statusLine, plugins и apiKeyHelper — код
// home актора. Проектные .claude/settings.json, CLAUDE.md, rules, agents,
// skills и commands остаются и приходят из подтверждённого commit: checkout
// отказывает, если на путях настройки Claude лежит ignored-файл или commit
// ставит на них ссылку (sessionctl, claudeConfigPathspecs). --strict-mcp-config
// оставляет только brood: MCP-серверы ~/.claude.json и .mcp.json не стартуют.
func stepAppArgs() []string {
	return []string{"claude", "--dangerously-skip-permissions", "--setting-sources", "project", "--strict-mcp-config", "--mcp-config", broodMCPConfig}
}

// stepAppEnv добавляет к окружению свободной сессии Git-конфигурацию root и
// bash: без SHELL Claude выбрал бы zsh, найдись он в образе, а его rc-файлы
// не приводятся. Имена зарезервированы projectsecret и не повторяются.
func stepAppEnv(env []string, run string) []string {
	return append(slices.Clone(env), "GIT_CONFIG_GLOBAL="+filepath.Join(run, stepGitConfigFile), "SHELL=/bin/bash")
}

// prepareClaudeLaunch — argv и окружение Claude для режима, который назначил
// root. В режиме шага до exec уходят в копии файлы home, чей env Claude Code
// 2.1.310 применяет к себе при любом --setting-sources: устаревшая глобальная
// конфигурация и кэш управляемых настроек в ~/.claude, затем env из
// ~/.claude.json. Отказ переноса не запускает Claude.
func prepareClaudeLaunch(l appLayout, warn io.Writer) ([]string, []string, error) {
	step, err := readStepSession(l.Run, l.RunUID, l.RunGID)
	if err != nil {
		return nil, nil, err
	}
	if step {
		now := time.Now()
		if err := asideClaudeHomeConfig(l.Home, now); err != nil {
			return nil, nil, err
		}
		if err := asideClaudeGlobalEnv(l.Home, now); err != nil {
			return nil, nil, err
		}
	}
	env, err := prepareProjectApp(l, warn)
	if err != nil {
		return nil, nil, err
	}
	if !step {
		return projectAppArgs(), env, nil
	}
	return stepAppArgs(), stepAppEnv(env, l.Run), nil
}

// claudeHomeAside — файлы ~/.claude, которые Claude Code 2.1.310 читает
// вместе с их env при любом --setting-sources (строки бинарника и запуск без
// сети с флагами stepAppArgs):
//   - .config.json — устаревшее место глобальной конфигурации. Если это
//     обычный файл (Claude идёт по ссылке) и он непуст или пишется при
//     отсутствии ~/.claude.json, Claude читает его вместо ~/.claude.json;
//   - remote-settings.json — кэш управляемых настроек с сервера. Пока сессия
//     для них подходит, Claude применяет кэш как управляемый уровень до
//     проверки и вместо drop-in /etc/claude-code (first-wins), так что
//     allowManagedHooksOnly пропал бы. С ANTHROPIC_BASE_URL brood сессия не
//     подходит, но признак зависит и от входа, сохранённого в home.
//
// Оба уходят в копии целиком, а не правятся: после переноса глобальная
// конфигурация Claude — один файл, ~/.claude.json, из которого env убирает
// asideClaudeGlobalEnv и в который wrapper пишет доверие проекту. Правка на
// месте оставила бы правило выбора Claude (размер, права записи, ссылка) и
// содержимое кэша условием безопасности. Законный кэш Claude получит заново
// с сервера. CLAUDE_CONFIG_DIR и CLAUDE_CODE_CUSTOM_OAUTH_URL, меняющие эти
// пути, в окружение сессии не попадают: имена CLAUDE_* projectsecret
// резервирует.
var claudeHomeAside = []string{".config.json", "remote-settings.json"}

// asideClaudeHomeConfig переносит claudeHomeAside в копии рядом, как rc-файлы:
// rename не читает содержимое и не идёт по ссылке. ~/.claude ссылкой или не
// каталогом актора — отказ: Claude прочитал бы .config.json за ней.
func asideClaudeHomeConfig(home string, now time.Time) error {
	fail := errors.New("~/.claude is a link or not a private directory of the actor, or its legacy global configuration or cached managed settings cannot be put aside; the step session does not start")
	dir, err := openSecretDirectory(home, false)
	if err != nil {
		return fail
	}
	defer unix.Close(dir)
	if secureSecretDirectory(dir, os.Geteuid(), false) != nil {
		return fail
	}
	claude, err := secretChildDirectory(dir, ".claude", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fail
	}
	defer unix.Close(claude)
	if secureSecretDirectory(claude, os.Geteuid(), false) != nil {
		return fail
	}
	if _, err := putAside(claude, claudeHomeAside, asideStamp(now)); err != nil {
		return fail
	}
	return nil
}

// asideClaudeGlobalEnv переносит член env из ~/.claude.json в
// ~/.claude.json.env.brood-<время>: сначала копия, потом файл без env, так что
// значение не теряется. Ссылка, чужой владелец или не-объект — отказ: Claude
// прочитал бы такой файл целиком.
func asideClaudeGlobalEnv(home string, now time.Time) error {
	fail := errors.New("~/.claude.json is a link, not a private regular JSON object, or its env cannot be put aside; the step session does not start")
	dir, err := openSecretDirectory(home, false)
	if err != nil || secureSecretDirectory(dir, os.Geteuid(), false) != nil {
		if err == nil {
			unix.Close(dir)
		}
		return fail
	}
	defer unix.Close(dir)
	data, _, err := readSecretFile(dir, ".claude.json", claudeConfigLimit, os.Geteuid(), 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fail
	}
	object, valid := jsonObject(data)
	if !valid && len(bytes.TrimSpace(data)) > 0 {
		return fail
	}
	value, present := object["env"]
	if !present {
		return nil
	}
	stamp, name := asideStamp(now), ""
	for attempt := 0; attempt < 100 && name == ""; attempt++ {
		var st unix.Stat_t
		if candidate := asideName(".claude.json.env", stamp, attempt); errors.Is(unix.Fstatat(dir, candidate, &st, unix.AT_SYMLINK_NOFOLLOW), unix.ENOENT) {
			name = candidate
		}
	}
	if name == "" || replaceSecretFile(dir, name, append(slices.Clone(value), '\n'), 0600, os.Geteuid(), os.Getegid(), true) != nil {
		return fail
	}
	delete(object, "env")
	out, err := json.MarshalIndent(object, "", "  ")
	if err != nil || replaceSecretFile(dir, ".claude.json", append(out, '\n'), 0600, os.Geteuid(), os.Getegid(), false) != nil {
		return fail
	}
	return nil
}

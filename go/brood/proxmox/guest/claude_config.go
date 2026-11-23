//go:build linux

package guest

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"strings"

	"golang.org/x/sys/unix"
)

const claudeConfigLimit = 1 << 20

var jsonTrue = json.RawMessage("true")

// autonomousDeny перечисляет встроенные инструменты Claude Code, которые
// автономному актору не нужны или вредны (профиль mop, #164). Правило deny
// убирает инструмент из списка модели, а не только отказывает при вызове, и
// под --dangerously-skip-permissions тоже (запуск закреплённой 2.1.310 без
// сети):
//   - SendMessage и ListAgents видят только сессии этого CT; сообщения роя
//     идут через MCP-сервер brood;
//   - AskUserQuestion паркует сессию: человека у терминала нет;
//   - EnterWorktree и ExitWorktree уводят работу из /workspace/project, а
//     файлы .claude/worktrees откажут следующему подтверждению шага;
//   - SendFeedback копит черновики отзыва, показывать их некому.
var autonomousDeny = []string{"SendMessage", "ListAgents", "AskUserQuestion", "EnterWorktree", "ExitWorktree", "SendFeedback"}

// autonomousSettings сливает в настройки Claude согласие на bypass и
// автономный профиль (#164): crossSessionInbound "accept" принимает сообщения
// других сессий без диалога Deliver/Deny, даже когда Claude не сверяет режим
// из конверта или сессия ещё стартует; feedbackDrafts "off" выключает
// черновики отзыва. Эти ключи принадлежат brood и ставятся при каждом старте;
// deny только дописывается, прочие ключи и записи актора остаются.
func autonomousSettings(o map[string]json.RawMessage) bool {
	changed := setJSON(o, "skipDangerousModePermissionPrompt", jsonTrue)
	changed = setJSON(o, "crossSessionInbound", json.RawMessage(`"accept"`)) || changed
	changed = setJSON(o, "feedbackDrafts", json.RawMessage(`"off"`)) || changed
	return denyTools(o, autonomousDeny) || changed
}

// denyTools дописывает в permissions.deny недостающие имена после записей
// актора; остальные члены permissions не меняются. permissions не-объектом или
// deny не-массивом Claude Code не прочитал бы: они заменяются.
func denyTools(o map[string]json.RawMessage, names []string) bool {
	permissions, valid := jsonObject(o["permissions"])
	var deny []json.RawMessage
	raw := bytes.TrimSpace(permissions["deny"])
	listed := len(raw) > 0 && raw[0] == '[' && json.Unmarshal(raw, &deny) == nil
	changed := !valid || !listed
	for _, name := range names {
		if !slices.ContainsFunc(deny, func(entry json.RawMessage) bool {
			var denied string
			return json.Unmarshal(entry, &denied) == nil && denied == name
		}) {
			quoted, _ := json.Marshal(name)
			deny, changed = append(deny, quoted), true
		}
	}
	if changed {
		permissions["deny"], _ = json.Marshal(deny)
		o["permissions"], _ = json.Marshal(permissions)
	}
	return changed
}

// prepareClaudeConfig keeps an unattended Claude off its onboarding (theme and
// security notes), long-resume, project-trust and bypass-mode dialogs and
// merges the autonomous profile into the user settings. Unknown user keys
// survive; both files are attempted even if one is refused.
func prepareClaudeConfig(home, project string) error {
	homeFD, err := openSecretDirectory(home, false)
	if err != nil {
		return errors.New("Claude home is unavailable or unsafe")
	}
	defer unix.Close(homeFD)
	if secureSecretDirectory(homeFD, os.Geteuid(), false) != nil {
		return errors.New("Claude home is unavailable or unsafe")
	}
	var reasons []string
	if err := mergeClaudeJSON(homeFD, ".claude.json", func(o map[string]json.RawMessage) bool { return unattendedConfig(o, project) }); err != nil {
		reasons = append(reasons, err.Error())
	}
	settings, err := openClaudeSettings(homeFD)
	if err != nil {
		reasons = append(reasons, "~/.claude is not a private directory; settings left unchanged")
	} else {
		defer unix.Close(settings)
		if err := mergeClaudeJSON(settings, "settings.json", autonomousSettings); err != nil {
			reasons = append(reasons, err.Error())
		}
	}
	if len(reasons) > 0 {
		return errors.New(strings.Join(reasons, "; "))
	}
	return nil
}

// seedClaudeModel writes only when Claude has no saved preference. In
// particular, the setting written by /model must survive a Brood restart.
func seedClaudeModel(home, model string) error {
	fd, err := openSecretDirectory(home, false)
	if err != nil {
		return errors.New("Claude home is unavailable or unsafe")
	}
	defer unix.Close(fd)
	if secureSecretDirectory(fd, os.Geteuid(), false) != nil {
		return errors.New("Claude home is unavailable or unsafe")
	}
	settings, err := openClaudeSettings(fd)
	if err != nil {
		return errors.New("~/.claude is unavailable or unsafe; settings left unchanged")
	}
	defer unix.Close(settings)
	initial, _ := json.Marshal(model)
	return mergeClaudeJSON(settings, "settings.json", func(o map[string]json.RawMessage) bool {
		if _, exists := o["model"]; exists {
			return false
		}
		return setJSON(o, "model", initial)
	})
}

func openClaudeSettings(homeFD int) (int, error) {
	settings, err := secretChildDirectory(homeFD, ".claude", true)
	if err != nil {
		return -1, err
	}
	if err := secureSecretDirectory(settings, os.Geteuid(), false); err != nil {
		unix.Close(settings)
		return -1, err
	}
	return settings, nil
}

func unattendedConfig(o map[string]json.RawMessage, project string) bool {
	changed := setJSON(o, "hasCompletedOnboarding", jsonTrue)
	changed = setJSON(o, "resumeReturnDismissed", jsonTrue) || changed
	var theme string
	if json.Unmarshal(o["theme"], &theme) != nil || theme == "" {
		o["theme"], changed = json.RawMessage(`"dark"`), true
	}
	projects, valid := jsonObject(o["projects"])
	entry, entryValid := jsonObject(projects[project])
	if setJSON(entry, "hasTrustDialogAccepted", jsonTrue) || !entryValid || !valid {
		projects[project], _ = json.Marshal(entry)
		o["projects"], _ = json.Marshal(projects)
		changed = true
	}
	return changed
}

func setJSON(o map[string]json.RawMessage, key string, value json.RawMessage) bool {
	if bytes.Equal(bytes.TrimSpace(o[key]), value) {
		return false
	}
	o[key] = value
	return true
}

// jsonObject keeps member values as raw bytes, so numbers and unknown values
// round-trip exactly. Anything but one object is reported invalid.
func jsonObject(data []byte) (map[string]json.RawMessage, bool) {
	var object map[string]json.RawMessage
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || trimmed[0] != '{' || json.Unmarshal(trimmed, &object) != nil || object == nil {
		return map[string]json.RawMessage{}, false
	}
	return object, true
}

// mergeClaudeJSON never writes through a symlink or hard link: the read refuses
// both, and the replacement is a fresh inode renamed over the name. A malformed
// file becomes the minimal object carrying our keys, never an empty file.
func mergeClaudeJSON(dir int, name string, edit func(map[string]json.RawMessage) bool) error {
	label := "~/" + name
	if name == "settings.json" {
		label = "~/.claude/settings.json"
	}
	data, _, err := readSecretFile(dir, name, claudeConfigLimit, os.Geteuid(), 0)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New(label + " is a link, not a private regular file, or exceeds 1 MiB; left unchanged")
	}
	object, valid := jsonObject(data)
	if !edit(object) && valid {
		return nil
	}
	out, err := json.MarshalIndent(object, "", "  ")
	if err != nil || len(out) >= claudeConfigLimit {
		return errors.New(label + " would exceed 1 MiB; left unchanged")
	}
	if replaceSecretFile(dir, name, append(out, '\n'), 0600, os.Geteuid(), os.Getegid(), false) != nil {
		return errors.New(label + " cannot be replaced atomically; left unchanged")
	}
	return nil
}

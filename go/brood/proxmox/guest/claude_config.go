//go:build linux

package guest

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

const claudeConfigLimit = 1 << 20

var jsonTrue = json.RawMessage("true")

// prepareClaudeConfig keeps an unattended Claude off its onboarding (theme and
// security notes), long-resume, project-trust and bypass-mode dialogs. Unknown
// user keys survive; both files are attempted even if one is refused.
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
	settings, err := secretChildDirectory(homeFD, ".claude", true)
	if err == nil && secureSecretDirectory(settings, os.Geteuid(), false) != nil {
		unix.Close(settings)
		err = secretFailure()
	}
	if err != nil {
		reasons = append(reasons, "~/.claude is not a private directory; settings left unchanged")
	} else {
		defer unix.Close(settings)
		if err := mergeClaudeJSON(settings, "settings.json", func(o map[string]json.RawMessage) bool {
			return setJSON(o, "skipDangerousModePermissionPrompt", jsonTrue)
		}); err != nil {
			reasons = append(reasons, err.Error())
		}
	}
	if len(reasons) > 0 {
		return errors.New(strings.Join(reasons, "; "))
	}
	return nil
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

//go:build linux

package guest

import (
	"encoding/json"
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"brood.dev/go/brood/actormodel"
	"brood.dev/go/brood/gitidentity"
	"brood.dev/go/brood/gitorigin"
)

const workspaceOriginFile = "workspace-origin.json"
const workspaceOriginLimit = 4 << 10

// WorkspaceOrigin is the workspace metadata projected to the app wrapper.
// The SSH private key is delivered separately and never serialized here.
type WorkspaceOrigin struct {
	Origin      string               `json:"origin"`
	GitIdentity gitidentity.Identity `json:"gitIdentity,omitzero"`
	Models      actormodel.Models    `json:"models,omitzero"`
}

func checkWorkspaceOrigin(o WorkspaceOrigin) error {
	if err := o.Models.Validate(); err != nil {
		return err
	}
	if o.Origin != "" {
		canonical, err := gitorigin.Canonical(o.Origin)
		if err != nil || canonical != o.Origin {
			return errors.New("workspace origin must be a canonical HTTPS or SSH repository URL")
		}
	}
	if !o.GitIdentity.IsZero() {
		if o.Origin == "" {
			return errors.New("workspace Git identity requires an origin")
		}
		return o.GitIdentity.Validate()
	}
	return nil
}

func originFailure() error { return errors.New("workspace origin projection unavailable or unsafe") }

func publishWorkspaceOrigin(path string, o WorkspaceOrigin, uid, gid int) error {
	if err := checkWorkspaceOrigin(o); err != nil {
		return err
	}
	data, err := json.Marshal(o)
	if err != nil || len(data) > workspaceOriginLimit {
		return originFailure()
	}
	dir, err := openSecretDirectory(path, false)
	if err != nil {
		return originFailure()
	}
	defer unix.Close(dir)
	if secureSecretDirectory(dir, uid, false) != nil {
		return originFailure()
	}
	if _, _, err = readSecretFile(dir, workspaceOriginFile, workspaceOriginLimit, uid, 0440); err != nil && !errors.Is(err, os.ErrNotExist) {
		return originFailure()
	}
	if replaceSecretFile(dir, workspaceOriginFile, data, 0440, uid, gid, false) != nil {
		return originFailure()
	}
	return nil
}

func readWorkspaceOrigin(path string, uid, gid int) (WorkspaceOrigin, error) {
	dir, err := openSecretDirectory(path, false)
	if err != nil {
		return WorkspaceOrigin{}, originFailure()
	}
	defer unix.Close(dir)
	if secureSecretDirectory(dir, uid, false) != nil {
		return WorkspaceOrigin{}, originFailure()
	}
	data, st, err := readSecretFile(dir, workspaceOriginFile, workspaceOriginLimit, uid, 0440)
	if err != nil || st.Gid != uint32(gid) {
		return WorkspaceOrigin{}, originFailure()
	}
	return decodeWorkspaceOrigin(data, nil)
}

const workspaceSSHKeyFile = "git-ssh-key"
const workspaceSSHKeyPath = projectSecretDirectory + "/" + workspaceSSHKeyFile
const sshClient = "/usr/bin/ssh"
const sshKnownHosts = "~/.ssh/known_hosts"
const workspaceSSHKeyLimit = 64 << 10

// TOFU: первый ключ сервера принимается и сохраняется в домашнем каталоге
// актора, а изменившийся ключ отклоняется. Ключ актора живёт только в /run.
const workspaceSSHConfig = `Match localuser ` + actorAccount + ` user git
    IdentityFile ` + workspaceSSHKeyPath + `
    IdentitiesOnly yes
    IdentityAgent none
    StrictHostKeyChecking accept-new
    UserKnownHostsFile ` + sshKnownHosts + `
    UpdateHostKeys no
Match all
`

// publishWorkspaceSSH writes the runtime key and a Debian SSH client drop-in.
// User SSH config and known_hosts stay untouched by the supervisor.
func publishWorkspaceSSH(key string) error {
	fail := func() error {
		return errors.New("workspace SSH credential setup failed; check guest runtime and OpenSSH configuration")
	}
	run, err := openSecretDirectory(projectSecretDirectory, false)
	if err != nil {
		return fail()
	}
	defer unix.Close(run)
	config, err := openSecretDirectory("/etc/ssh/ssh_config.d", false)
	if err != nil {
		return fail()
	}
	defer unix.Close(config)
	if secureSecretDirectory(run, 0, false) != nil || secureSecretDirectory(config, 0, false) != nil {
		return fail()
	}
	const configName = "00-brood-workspace.conf"
	if key == "" {
		for _, file := range []struct {
			dir  int
			name string
		}{{run, workspaceSSHKeyFile}, {config, configName}} {
			if err := unix.Unlinkat(file.dir, file.name, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
				return fail()
			}
			if unix.Fsync(file.dir) != nil {
				return fail()
			}
		}
		return nil
	}
	if !strings.HasSuffix(key, "\n") {
		key += "\n"
	}
	if replaceSecretFile(run, workspaceSSHKeyFile, []byte(key), 0600, 1000, 1000, false) != nil ||
		replaceSecretFile(config, configName, []byte(workspaceSSHConfig), 0644, 0, 0, false) != nil {
		return fail()
	}
	return nil
}

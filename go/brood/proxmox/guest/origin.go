//go:build linux

package guest

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// workspaceOriginFile lives next to the secret projection in /run/brood but is
// not secret: root writes the bootstrap origin there, the app wrapper reads it.
const workspaceOriginFile = "workspace-origin.json"
const originLimit = 1024

// WorkspaceOrigin is the clone source of an actor: both fields empty, or a
// canonical HTTPS origin with mode gateway (Brood Git gateway) or direct.
type WorkspaceOrigin struct {
	Origin string `json:"origin"`
	Mode   string `json:"mode"`
}

// ValidateWorkspaceOrigin checks the bootstrap pair. It mirrors the server's
// canonical-origin contract locally; a direct origin must be an approved host.
func ValidateWorkspaceOrigin(origin, mode string, hosts []string) error {
	if err := checkWorkspaceOrigin(WorkspaceOrigin{Origin: origin, Mode: mode}); err != nil || mode != "direct" {
		return err
	}
	u, _ := url.Parse(origin)
	for _, host := range hosts {
		if approved, err := canonicalHost(host); err == nil && approved == u.Hostname() {
			return nil
		}
	}
	return errors.New("direct workspace origin host is not an approved guest host")
}

func checkWorkspaceOrigin(o WorkspaceOrigin) error {
	switch {
	case o.Origin == "" && o.Mode == "":
		return nil
	case o.Origin == "" || o.Mode == "":
		return errors.New("workspace origin and mode must be set together")
	case o.Mode != "gateway" && o.Mode != "direct":
		return errors.New("workspace origin mode must be gateway or direct")
	case !canonicalOrigin(o.Origin):
		return errors.New("workspace origin must be a canonical HTTPS repository URL")
	}
	return nil
}

// canonicalOrigin: https only, no userinfo, query, fragment or port other than
// 443, lowercase DNS hostname (not an IP), nonempty path without a trailing
// slash, no control characters, at most 1024 bytes.
func canonicalOrigin(origin string) bool {
	if len(origin) > originLimit || strings.Contains(origin, "#") {
		return false
	}
	for i := 0; i < len(origin); i++ {
		if origin[i] < 0x20 || origin[i] == 0x7f {
			return false
		}
	}
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	if !strings.HasPrefix(origin, "https://"+u.Host+"/") || u.Path == "/" || strings.HasSuffix(u.Path, "/") {
		return false
	}
	return dnsHostname(u.Hostname())
}

func dnsHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 || net.ParseIP(host) != nil {
		return false
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	// Числовая последняя метка — форма IP (1.2.3, 0x7f.1), а не имя.
	return strings.ContainsAny(labels[len(labels)-1], "abcdefghijklmnopqrstuvwxyz")
}

// PublishWorkspaceOrigin stages the bootstrap origin for the app wrapper as
// root:1000 0440. Only guest root may call it, before the actor starts.
func PublishWorkspaceOrigin(origin, mode string) error {
	if os.Geteuid() != 0 {
		return errors.New("workspace origin requires guest root")
	}
	if err := prepareRunDirectory(); err != nil {
		return err
	}
	return publishWorkspaceOrigin(projectSecretDirectory, origin, mode, 0, 1000)
}

func originFailure() error { return errors.New("workspace origin projection unavailable or unsafe") }

func publishWorkspaceOrigin(path, origin, mode string, uid, gid int) error {
	o := WorkspaceOrigin{Origin: origin, Mode: mode}
	if err := checkWorkspaceOrigin(o); err != nil {
		return err
	}
	data, err := json.Marshal(o)
	if err != nil {
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
	if _, _, err = readSecretFile(dir, workspaceOriginFile, 4<<10, uid, 0440); err != nil && !errors.Is(err, os.ErrNotExist) {
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
	data, st, err := readSecretFile(dir, workspaceOriginFile, 4<<10, uid, 0440)
	if err != nil || st.Gid != uint32(gid) {
		return WorkspaceOrigin{}, originFailure()
	}
	var o WorkspaceOrigin
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&o) != nil || decoder.Decode(new(any)) != io.EOF || checkWorkspaceOrigin(o) != nil {
		return WorkspaceOrigin{}, originFailure()
	}
	return o, nil
}

//go:build linux

package guest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"brood.dev/go/brood/projectsecret"
	"brood.dev/go/brood/proxmox/relay"
	"golang.org/x/sys/unix"
)

const projectSecretDirectory = "/run/brood"
const projectSecretLimit = 4 << 20

func secretFailure() error { return errors.New("project secrets unavailable or unsafe") }

func decodeProjectSecrets(data []byte) (projectsecret.Bundle, error) {
	var bundle projectsecret.Bundle
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if len(data) > projectSecretLimit || decoder.Decode(&bundle) != nil || decoder.Decode(new(any)) != io.EOF || projectsecret.ValidateBundle(bundle) != nil {
		return projectsecret.Bundle{}, secretFailure()
	}
	return bundle, nil
}

// FetchProjectSecrets uses only the root bootstrap identity, never a relay socket
// or actor environment. Errors deliberately omit URLs, tokens and response bodies.
func FetchProjectSecrets(ctx context.Context, cfg relay.ClientConfig) (projectsecret.Bundle, error) {
	fail := func() (projectsecret.Bundle, error) { return projectsecret.Bundle{}, secretFailure() }
	target, err := url.Parse(cfg.URL)
	if err != nil || target.Scheme != "wss" || target.Hostname() == "" || target.User != nil || target.RawQuery != "" || target.ForceQuery || target.Fragment != "" || target.RawPath != "" || len(cfg.Token) != 43 || strings.ContainsAny(cfg.Token, "\r\n\x00") || cfg.Generation <= 0 || len(cfg.ActorUID) == 0 || len(cfg.ActorUID) > 128 {
		return fail()
	}
	for _, r := range cfg.ActorUID {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return fail()
		}
	}
	generation := strconv.FormatInt(cfg.Generation, 10)
	if target.Path != "" && target.Path != "/" && target.Path != "/api/proxmox/guest/"+cfg.ActorUID+"/"+generation {
		return fail()
	}
	target.Scheme = "https"
	target.Path = "/api/proxmox/secrets/" + cfg.ActorUID + "/" + generation
	roots, err := x509.SystemCertPool()
	if err != nil {
		return fail()
	}
	if len(cfg.CAPEM) > 0 {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(cfg.CAPEM) {
			return fail()
		}
	}
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 5 * time.Second, DisableKeepAlives: true, MaxResponseHeaderBytes: 32 << 10, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: target.Hostname()}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return secretFailure() }}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return fail()
	}
	req.Header.Set("Authorization", "Bearer "+cfg.Token)
	response, err := client.Do(req)
	if err != nil {
		return fail()
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fail()
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, projectSecretLimit+1))
	if err != nil {
		return fail()
	}
	return decodeProjectSecrets(data)
}

// RefreshProjectSecrets fetches and atomically stages the next app's bundle.
// Only guest root may call it; the projection never contains machine credentials.
func RefreshProjectSecrets(ctx context.Context, cfg relay.ClientConfig) error {
	if os.Geteuid() != 0 {
		return secretFailure()
	}
	bundle, err := FetchProjectSecrets(ctx, cfg)
	if err != nil {
		return secretFailure()
	}
	// /run is root-controlled; never create this directory through a symlink.
	run, err := openSecretDirectory("/run", false)
	if err != nil {
		return secretFailure()
	}
	defer unix.Close(run)
	if err = secureSecretDirectory(run, 0, false); err != nil {
		return err
	}
	if err = unix.Mkdirat(run, "brood", 0750); err != nil && !errors.Is(err, unix.EEXIST) {
		return secretFailure()
	}
	dir, err := unix.Openat(run, "brood", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return secretFailure()
	}
	defer unix.Close(dir)
	if err = secureSecretDirectory(dir, 0, false); err != nil {
		return err
	}
	if unix.Fchown(dir, 0, 1000) != nil || unix.Fchmod(dir, 0750) != nil {
		return secretFailure()
	}
	return publishProjectSecrets(projectSecretDirectory, bundle, 0, 1000)
}

func publishProjectSecrets(path string, bundle projectsecret.Bundle, uid, gid int) error {
	if projectsecret.ValidateBundle(bundle) != nil {
		return secretFailure()
	}
	data, err := json.Marshal(bundle)
	if err != nil || len(data) > projectSecretLimit {
		return secretFailure()
	}
	dir, err := openSecretDirectory(path, false)
	if err != nil {
		return secretFailure()
	}
	defer unix.Close(dir)
	if secureSecretDirectory(dir, uid, false) != nil {
		return secretFailure()
	}
	// Existing projections must have the exact root-only write contract too.
	if _, _, err = readSecretFile(dir, "project-secrets.json", projectSecretLimit, uid, 0440); err != nil && !errors.Is(err, os.ErrNotExist) {
		return secretFailure()
	}
	return replaceSecretFile(dir, "project-secrets.json", data, 0440, uid, gid, false)
}
func readProjectSecrets(path string, uid, gid int) (projectsecret.Bundle, error) {
	dir, err := openSecretDirectory(path, false)
	if err != nil {
		return projectsecret.Bundle{}, secretFailure()
	}
	defer unix.Close(dir)
	if secureSecretDirectory(dir, uid, false) != nil {
		return projectsecret.Bundle{}, secretFailure()
	}
	data, st, err := readSecretFile(dir, "project-secrets.json", projectSecretLimit, uid, 0440)
	if err != nil || st.Gid != uint32(gid) {
		return projectsecret.Bundle{}, secretFailure()
	}
	return decodeProjectSecrets(data)
}

func projectAppEnvironment(bundle projectsecret.Bundle) ([]string, error) {
	if projectsecret.ValidateBundle(bundle) != nil {
		return nil, secretFailure()
	}
	env := actorEnv()
	fixed := map[string]bool{}
	for _, item := range env {
		name, _, _ := strings.Cut(item, "=")
		fixed[strings.ToUpper(name)] = true
	}
	names := make([]string, 0, len(bundle.Vars))
	for name := range bundle.Vars {
		if fixed[strings.ToUpper(name)] {
			return nil, secretFailure()
		}
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		env = append(env, name+"="+bundle.Vars[name])
	}
	return env, nil
}

// RunProjectApp is the only project-secret consumer. Dispatch before reading root
// configuration. No caller-selected path, command, or environment is accepted.
func RunProjectApp() error {
	if os.Getuid() != 1000 || os.Geteuid() != 1000 || os.Getgid() != 1000 || os.Getegid() != 1000 {
		return secretFailure()
	}
	groups, err := os.Getgroups()
	if err != nil {
		return secretFailure()
	}
	for _, g := range groups {
		if g != 1000 {
			return secretFailure()
		}
	}
	if err = lockPrivileges(); err != nil {
		return secretFailure()
	}
	unix.Umask(0077)
	bundle, err := readProjectSecrets(projectSecretDirectory, 0, 1000)
	if err != nil {
		return err
	}
	env, err := applyProjectSecrets("/workspace/project", "/workspace/home", bundle)
	if err != nil {
		return err
	}
	if os.Chdir("/workspace/project") != nil {
		return secretFailure()
	}
	return syscall.Exec("/usr/local/bin/claude", projectAppArgs(), env)
}

func projectAppArgs() []string {
	return []string{"claude", "--permission-mode", "default", "--mcp-config", `{"mcpServers":{"brood":{"command":"/usr/local/bin/bro","args":["--guest","mcp"]}}}`}
}

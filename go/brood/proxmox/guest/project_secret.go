//go:build linux

package guest

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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
	if err = prepareRunDirectory(); err != nil {
		return err
	}
	return publishProjectSecrets(projectSecretDirectory, bundle, 0, 1000)
}

// prepareRunDirectory creates /run/brood as root:1000 0750 for the app wrapper.
func prepareRunDirectory() error {
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
	return nil
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
// Nonfatal startup problems are reported to warn, the terminal of the session.
func RunProjectApp(warn io.Writer) error {
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
	layout := appLayout{Project: "/workspace/project", Home: "/workspace/home", Run: projectSecretDirectory, RunUID: 0, RunGID: 1000, Gateway: workspaceGatewayURL, CloneTimeout: 30 * time.Minute}
	env, err := prepareProjectApp(layout, warn)
	if err != nil {
		return err
	}
	if os.Chdir(layout.Project) != nil {
		return secretFailure()
	}
	return syscall.Exec("/usr/local/bin/claude", projectAppArgs(), env)
}

// appLayout fixes every path of the app wrapper; tests substitute temporary ones.
type appLayout struct {
	Project, Home, Run string
	RunUID, RunGID     int
	Gateway            string
	CloneTimeout       time.Duration
}

// prepareProjectApp clones a fresh workspace before file secrets, which need a
// checkout, and keeps Claude off its interactive dialogs. A failed clone or
// refused Claude configuration still starts the app.
func prepareProjectApp(l appLayout, warn io.Writer) ([]string, error) {
	origin, err := readWorkspaceOrigin(l.Run, l.RunUID, l.RunGID)
	if err != nil {
		fmt.Fprintln(warn, "brood: workspace origin is unavailable; the first-start clone is skipped")
	} else if err = cloneWorkspace(context.Background(), cloneSpec{Project: l.Project, Home: l.Home, Origin: origin.Origin, Mode: origin.Mode, Gateway: l.Gateway, Timeout: l.CloneTimeout}); err != nil {
		fmt.Fprintf(warn, "brood: %s; Claude starts now and the clone is retried on the next start while the workspace is empty\n", err)
	}
	bundle, err := readProjectSecrets(l.Run, l.RunUID, l.RunGID)
	if err != nil {
		return nil, err
	}
	env, err := applyProjectSecrets(l.Project, l.Home, bundle)
	if err != nil && len(bundle.Files) > 0 && missingCheckout(l.Project) {
		// Без checkout байты файлов не публикуются; переменные всё равно нужны.
		fmt.Fprintln(warn, "brood: project file secrets need a Git checkout in the workspace; Claude starts without them")
		bundle.Files = nil
		env, err = applyProjectSecrets(l.Project, l.Home, bundle)
	}
	if err != nil {
		return nil, err
	}
	if err := prepareClaudeConfig(l.Home, l.Project); err != nil {
		fmt.Fprintf(warn, "brood: %s; Claude may stop on its first-run dialogs\n", err)
	}
	return env, nil
}

func missingCheckout(project string) bool {
	_, err := os.Lstat(filepath.Join(project, ".git"))
	return os.IsNotExist(err)
}

func projectAppArgs() []string {
	return []string{"claude", "--dangerously-skip-permissions", "--mcp-config", `{"mcpServers":{"brood":{"command":"/usr/local/bin/bro","args":["--guest","mcp"]}}}`}
}

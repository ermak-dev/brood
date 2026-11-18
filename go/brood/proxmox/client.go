// Package proxmox provides a scoped, API-only client for owned PVE containers.
package proxmox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const maxBody = 1 << 20

var (
	ErrNotFound   = errors.New("PVE resource not found")
	ErrPermission = errors.New("PVE permission denied")
	ErrOwnership  = errors.New("PVE container ownership check failed")
	ErrCollision  = errors.New("PVE VMID conflict; reconcile before allocating again")
	ErrTaskFailed = errors.New("PVE task failed")
	ErrUncertain  = errors.New("PVE operation outcome uncertain; reconciliation required")
)

// Config fixes all placement boundaries. Empty CAPEM uses system trust roots.
// TemplateStorage and TemplatePrefix explicitly authorize an owned vztmpl namespace;
// the caller must publish and verify the template artifact, not pass an OCI URI.
// Credentials are excluded from JSON and both ordinary and Go-syntax formatting.
type Config struct {
	Endpoint        string
	TokenID         string `json:"-"`
	TokenSecret     string `json:"-"`
	CAPEM           string `json:"-"`
	Node            string
	ResourcePool    string
	Storage         string
	Network         string
	TemplateStorage string
	TemplatePrefix  string
}

func (Config) String() string     { return "proxmox.Config{redacted}" }
func (c Config) GoString() string { return c.String() }

// Format serves every verb: String and GoString cover only some, and the rest
// would print the fields. %p bypasses Format and prints an address only.
func (c Config) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, c.String()) }

// Ownership is a durable identity, not a hostname or a reusable VMID. The caller
// generates and persists the nonce and operation intent before any PVE write.
type Ownership struct {
	PoolUID     string `json:"poolUID"`
	ActorUID    string `json:"actorUID"`
	Incarnation uint64 `json:"incarnation"`
	Nonce       string `json:"nonce"`
}

// OwnedDescription encodes the complete identity in a versioned structured marker.
// Invalid identities yield an empty marker, which never authorizes a mutation.
func OwnedDescription(o Ownership) string {
	if !o.valid() {
		return ""
	}
	b, _ := json.Marshal(o)
	return "brood-owned:v1:" + string(b)
}
func (o Ownership) valid() bool {
	return identity.MatchString(o.PoolUID) && identity.MatchString(o.ActorUID) && identity.MatchString(o.Nonce) && o.Incarnation > 0
}

// BootstrapEnv contains only Brood guest-bootstrap variables. Values are secrets:
// formatting and JSON serialization deliberately never disclose the map contents.
// PVE receives a deterministic NUL-separated KEY=value list, not shell syntax.
type BootstrapEnv map[string]string

func (BootstrapEnv) String() string     { return "proxmox.BootstrapEnv{redacted}" }
func (e BootstrapEnv) GoString() string { return e.String() }

// Format serves every verb: String and GoString cover only some, and the rest
// would print the fields. %p bypasses Format and prints an address only.
func (e BootstrapEnv) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, e.String()) }
func (BootstrapEnv) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// CTSpec exposes no raw PVE options. Resource bounds are 1..128 cores,
// 16..1048576 MiB memory and 1..16384 GiB disk. MAC, if omitted, is derived from
// the full ownership identity and remains stable across request retries.
type CTSpec struct {
	VMID      int
	Hostname  string
	Template  string
	Owner     Ownership
	Cores     int
	MemoryMiB int
	DiskGiB   int
	MAC       string
	Env       BootstrapEnv `json:"-"`
}

func (CTSpec) String() string     { return "proxmox.CTSpec{redacted}" }
func (s CTSpec) GoString() string { return s.String() }

// Format serves every verb: String and GoString cover only some, and the rest
// would print the fields. %p bypasses Format and prints an address only.
func (c CTSpec) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, c.String()) }

// OwnedCT identifies the exact durable allocation to operate on.
type OwnedCT struct {
	VMID  int
	Owner Ownership
}

// CTConfig is a public allowlist. Raw env/lxc.environment fields are not retained.
type CTConfig struct {
	Description  string `json:"description"`
	Digest       string `json:"digest"`
	Hostname     string `json:"hostname"`
	Unprivileged int    `json:"unprivileged"`
	RootFS       string `json:"rootfs"`
}

// CTStatus reports PVE runtime status only, never guest or actor readiness.
type CTStatus struct {
	Status string `json:"status"`
}

// Client owns a verified TLS transport and never follows redirects or retries
// writes. It does not persist operation intent, change network policy, or check
// actor readiness. Callers reconcile uncertain outcomes using durable VMID/owner.
type Client struct {
	cfg  Config
	http *http.Client
}

func (*Client) String() string     { return "proxmox.Client{redacted}" }
func (c *Client) GoString() string { return c.String() }

// Format serves every verb: String and GoString cover only some, and the rest
// would print the fields. %p bypasses Format and prints an address only.
func (c *Client) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, c.String()) }

var (
	name         = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	identity     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)
	tokenID      = regexp.MustCompile(`^[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+![A-Za-z0-9_.-]+$`)
	hostname     = regexp.MustCompile(`^[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*$`)
	configDigest = regexp.MustCompile(`^[0-9a-f]{40}$`)
	envKey       = regexp.MustCompile(`^BROOD_GUEST_[A-Z][A-Z0-9_]{0,95}$`)
	templateFile = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,254}$`)
	upidPattern  = regexp.MustCompile(`^UPID:([A-Za-z0-9_-]+):[0-9A-Fa-f]{8,16}:[0-9A-Fa-f]{8,16}:[0-9A-Fa-f]{8,16}:[A-Za-z0-9_-]+:[A-Za-z0-9_-]*:[A-Za-z0-9_.@!-]+:$`)
)

// NewClient validates placement and credentials without contacting PVE. Endpoint
// must be an HTTPS origin; CA and hostname checks cannot be disabled.
func NewClient(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.ContainsAny(cfg.Endpoint, "\r\n\t ") {
		return nil, errors.New("PVE endpoint must be an HTTPS origin")
	}
	if p := u.Port(); p != "" {
		n, e := strconv.Atoi(p)
		if e != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid PVE endpoint port")
		}
	}
	if !tokenID.MatchString(cfg.TokenID) || !headerSafe(cfg.TokenSecret) {
		return nil, errors.New("invalid PVE token credentials")
	}
	for _, v := range []string{cfg.Node, cfg.ResourcePool, cfg.Storage, cfg.Network, cfg.TemplateStorage} {
		if !name.MatchString(v) {
			return nil, errors.New("invalid PVE placement configuration")
		}
	}
	if !templateFile.MatchString(cfg.TemplatePrefix) || strings.Contains(cfg.TemplatePrefix, "..") {
		return nil, errors.New("explicit owned PVE template prefix is required")
	}
	var roots *x509.CertPool
	if cfg.CAPEM != "" {
		roots = x509.NewCertPool()
		rest := []byte(cfg.CAPEM)
		count := 0
		for len(bytes.TrimSpace(rest)) > 0 {
			rest = bytes.TrimSpace(rest)
			if !bytes.HasPrefix(rest, []byte("-----BEGIN CERTIFICATE-----")) {
				return nil, errors.New("invalid PVE CA certificate bundle")
			}
			block, remaining := pem.Decode(rest)
			if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
				return nil, errors.New("invalid PVE CA certificate bundle")
			}
			cert, e := x509.ParseCertificate(block.Bytes)
			if e != nil || !cert.IsCA {
				return nil, errors.New("invalid PVE CA certificate bundle")
			}
			roots.AddCert(cert)
			count++
			rest = remaining
		}
		if count == 0 {
			return nil, errors.New("empty PVE CA certificate bundle")
		}
	}
	cfg.Endpoint = strings.TrimSuffix(cfg.Endpoint, "/")
	transport := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second, MaxResponseHeaderBytes: 32 << 10, MaxIdleConns: 4, MaxIdleConnsPerHost: 4, IdleConnTimeout: 60 * time.Second, DisableCompression: true}
	return &Client{cfg: cfg, http: &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func headerSafe(s string) bool {
	if len(s) == 0 || len(s) > 4096 {
		return false
	}
	for _, b := range []byte(s) {
		if b < 33 || b > 126 {
			return false
		}
	}
	return true
}
func validVMID(id int) bool { return id >= 100 && id <= 999999999 }

// Close releases idle transport connections. In-flight requests remain governed
// by their own contexts.
func (c *Client) Close() { c.http.CloseIdleConnections() }

type responseStatusError struct{ code int }

func (e responseStatusError) Error() string { return fmt.Sprintf("PVE HTTP status %d", e.code) }

func (c *Client) request(ctx context.Context, method, path string, form url.Values, out any) error {
	return c.requestData(ctx, method, path, form, out, false)
}

func (c *Client) requestData(ctx context.Context, method, path string, form url.Values, out any, allowNull bool) error {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.cfg.Endpoint+"/api2/json"+path, body)
	if err != nil {
		return errors.New("invalid PVE request")
	}
	req.Header.Set("Authorization", "PVEAPIToken="+c.cfg.TokenID+"="+c.cfg.TokenSecret)
	req.Header.Set("Accept", "application/json")
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("PVE request canceled: %w", ctx.Err())
		}
		return errors.New("PVE HTTPS request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		switch resp.StatusCode {
		case 404:
			return ErrNotFound
		case 401, 403:
			return ErrPermission
		case 409:
			return ErrCollision
		}
		return responseStatusError{code: resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	// A body can end cleanly but truncated after cancellation; never parse it.
	if ctx.Err() != nil {
		return fmt.Errorf("PVE response canceled: %w", ctx.Err())
	}
	if err != nil {
		return errors.New("PVE response read failed")
	}
	if len(raw) > maxBody {
		return errors.New("PVE response exceeds size limit")
	}
	data, err := envelopeData(raw, allowNull)
	if err != nil {
		return err
	}
	if json.Unmarshal(data, out) != nil {
		return errors.New("invalid PVE response data")
	}
	return nil
}
func envelopeData(raw []byte, allowNull bool) (json.RawMessage, error) {
	fail := errors.New("invalid PVE JSON envelope")
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, fail
	}
	seen := map[string]bool{}
	var data json.RawMessage
	for d.More() {
		tok, err = d.Token()
		if err != nil {
			return nil, fail
		}
		key, ok := tok.(string)
		if !ok || seen[key] {
			return nil, fail
		}
		seen[key] = true
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return nil, fail
		}
		if key == "data" {
			data = v
		}
		if key == "errors" && string(v) != "null" && string(v) != "{}" {
			return nil, fail
		}
	}
	if _, err = d.Token(); err != nil {
		return nil, fail
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, fail
	}
	if len(data) == 0 || (!allowNull && bytes.Equal(bytes.TrimSpace(data), []byte("null"))) {
		return nil, fail
	}
	return data, nil
}

// NextID obtains a suggestion, not a reservation. A create conflict is never
// retried with another VMID; intent and collision recovery belong to the caller.
func (c *Client) NextID(ctx context.Context) (int, error) {
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodGet, "/cluster/nextid", nil, &raw); err != nil {
		return 0, err
	}
	return parseVMID(raw)
}

func parseVMID(raw json.RawMessage) (int, error) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		s = string(raw)
	}
	id, err := strconv.Atoi(s)
	if err != nil || !validVMID(id) || strconv.Itoa(id) != s {
		return 0, errors.New("invalid PVE next VMID")
	}
	return id, nil
}

// ConfirmAbsent positively checks the global VMID registry, not ACL-filtered
// inventory. Only a successful exact-ID response proves absence at this instant;
// all errors retain the reservation. A rejected exact-ID candidate returns
// ErrCollision, not absence or permission to retarget a submitted create.
// It does not reserve the now-free VMID.
func (c *Client) ConfirmAbsent(ctx context.Context, id int) error {
	if !validVMID(id) {
		return errors.New("invalid PVE VMID")
	}
	var raw json.RawMessage
	if err := c.request(ctx, http.MethodGet, "/cluster/nextid?"+url.Values{"vmid": {strconv.Itoa(id)}}.Encode(), nil, &raw); err != nil {
		var status responseStatusError
		if errors.As(err, &status) && status.code == http.StatusBadRequest {
			return ErrCollision
		}
		return err
	}
	free, err := parseVMID(raw)
	if err != nil || free != id {
		return errors.New("PVE VMID absence was not confirmed")
	}
	return nil
}

// Create submits exactly once and returns the UPID for durable persistence.
// It never sets tags: PVE 9.1 checks VM.Config.Options before joining the pool.
// A missing/malformed success response or upstream server failure is uncertain,
// not proof of absence. Do not blindly retry create after an uncertain result.
func (c *Client) Create(ctx context.Context, s CTSpec) (string, error) {
	form, err := c.createForm(s)
	if err != nil {
		return "", err
	}
	return c.writeTask(ctx, http.MethodPost, "/nodes/"+c.cfg.Node+"/lxc", form)
}
func (c *Client) createForm(s CTSpec) (url.Values, error) {
	fail := errors.New("invalid PVE container specification")
	if !validVMID(s.VMID) || !s.Owner.valid() || !hostname.MatchString(s.Hostname) || len(s.Hostname) > 253 || s.Cores < 1 || s.Cores > 128 || s.MemoryMiB < 16 || s.MemoryMiB > 1048576 || s.DiskGiB < 1 || s.DiskGiB > 16384 {
		return nil, fail
	}
	prefix := c.cfg.TemplateStorage + ":vztmpl/"
	file, ok := strings.CutPrefix(s.Template, prefix)
	if !ok || !templateFile.MatchString(file) || strings.Contains(file, "..") || !strings.HasPrefix(file, c.cfg.TemplatePrefix) || len(s.Template) > 255 {
		return nil, errors.New("template is outside the configured owned vztmpl namespace")
	}
	mac := s.MAC
	if mac == "" {
		sum := sha256.Sum256([]byte(OwnedDescription(s.Owner)))
		mac = net.HardwareAddr{2, sum[0], sum[1], sum[2], sum[3], sum[4]}.String()
	}
	parsed, err := net.ParseMAC(mac)
	if err != nil || len(parsed) != 6 || parsed[0]&1 != 0 || bytes.Equal(parsed, make([]byte, 6)) {
		return nil, fail
	}
	mac = strings.ToUpper(parsed.String())
	env, err := c.bootstrapEnv(s.Env)
	if err != nil {
		return nil, err
	}
	form := url.Values{"vmid": {strconv.Itoa(s.VMID)}, "hostname": {s.Hostname}, "ostemplate": {s.Template}, "description": {OwnedDescription(s.Owner)}, "pool": {c.cfg.ResourcePool}, "storage": {c.cfg.Storage}, "unprivileged": {"1"}, "rootfs": {c.cfg.Storage + ":" + strconv.Itoa(s.DiskGiB)}, "cores": {strconv.Itoa(s.Cores)}, "memory": {strconv.Itoa(s.MemoryMiB)}, "swap": {"0"}, "start": {"0"}, "net0": {"name=eth0,bridge=" + c.cfg.Network + ",ip=dhcp,hwaddr=" + mac}}
	if env != "" {
		form.Set("env", env)
	}
	return form, nil
}

func (c *Client) bootstrapEnv(env BootstrapEnv) (string, error) {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pairs := make([]string, 0, len(keys))
	total := 0
	for _, k := range keys {
		v := env[k]
		if !envKey.MatchString(k) || len(v) > 8192 || strings.ContainsAny(v, "\x00\r\n") || strings.Contains(v, c.cfg.TokenSecret) || strings.Contains(v, c.cfg.TokenID) {
			return "", errors.New("invalid PVE guest bootstrap environment")
		}
		for _, r := range v {
			if r < 32 || r == 127 {
				return "", errors.New("invalid PVE guest bootstrap environment")
			}
		}
		total += len(k) + len(v) + 2
		if total > 64<<10 {
			return "", errors.New("PVE guest bootstrap environment exceeds size limit")
		}
		pairs = append(pairs, k+"="+v)
	}
	return strings.Join(pairs, "\x00"), nil
}
func (c *Client) ctPath(id int) string { return "/nodes/" + c.cfg.Node + "/lxc/" + strconv.Itoa(id) }

// GetConfig returns only allowlisted public fields, ignoring all raw env fields.
func (c *Client) GetConfig(ctx context.Context, id int) (CTConfig, error) {
	cfg, _, err := c.getConfig(ctx, id, false)
	return cfg, err
}

// InspectOwned verifies both config views, consistent digests and current pool
// membership before returning sanitized configuration for restart adoption. Like
// mutations, it cannot make pool membership atomic with a later external action.
func (c *Client) InspectOwned(ctx context.Context, ref OwnedCT) (CTConfig, error) {
	digest, err := c.ownedDigest(ctx, ref, true)
	if err != nil {
		return CTConfig{}, err
	}
	cfg, raw, err := c.getConfig(ctx, ref.VMID, false)
	if err != nil {
		return CTConfig{}, err
	}
	if cfg.Digest != digest {
		return CTConfig{}, ErrOwnership
	}
	if err := c.validateOwnedConfig(ref, cfg, raw); err != nil {
		return CTConfig{}, err
	}
	return cfg, nil
}

// Config is an alias for GetConfig.
func (c *Client) Config(ctx context.Context, id int) (CTConfig, error) { return c.GetConfig(ctx, id) }
func (c *Client) getConfig(ctx context.Context, id int, current bool) (CTConfig, map[string]json.RawMessage, error) {
	var cfg CTConfig
	if !validVMID(id) {
		return cfg, nil, errors.New("invalid PVE VMID")
	}
	var raw map[string]json.RawMessage
	path := c.ctPath(id) + "/config"
	if current {
		path += "?current=1"
	}
	if err := c.request(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return cfg, nil, err
	}
	// Do not retain raw environment values in any public result or error.
	delete(raw, "env")
	delete(raw, "lxc.environment.runtime")
	delete(raw, "lxc.environment")
	delete(raw, "lxc.environment.hooks")
	b, _ := json.Marshal(raw)
	if json.Unmarshal(b, &cfg) != nil {
		return CTConfig{}, nil, errors.New("invalid PVE container configuration")
	}
	return cfg, raw, nil
}

// Status reads runtime status without asserting guest readiness.
func (c *Client) Status(ctx context.Context, id int) (CTStatus, error) {
	var out CTStatus
	if !validVMID(id) {
		return out, errors.New("invalid PVE VMID")
	}
	err := c.request(ctx, http.MethodGet, c.ctPath(id)+"/status/current", nil, &out)
	if err != nil {
		return CTStatus{}, err
	}
	if out.Status != "running" && out.Status != "stopped" {
		err = errors.New("invalid PVE container status")
		out = CTStatus{}
	}
	return out, err
}

func (c *Client) assertOwned(ctx context.Context, ref OwnedCT) error {
	_, err := c.ownedDigest(ctx, ref, false)
	return err
}

func (c *Client) ownedDigest(ctx context.Context, ref OwnedCT, requireCAS bool) (string, error) {
	if !validVMID(ref.VMID) || !ref.Owner.valid() {
		return "", ErrOwnership
	}
	// PVE's default config view overlays pending values. Both must authorize the
	// mutation; matching digests bind the two views to the same config file.
	var digest string
	for _, current := range []bool{true, false} {
		cfg, raw, err := c.getConfig(ctx, ref.VMID, current)
		if err != nil {
			return "", err
		}
		if err := c.validateOwnedConfig(ref, cfg, raw); err != nil {
			return "", err
		}
		if requireCAS && (!configDigest.MatchString(cfg.Digest) || (!current && cfg.Digest != digest)) {
			return "", errors.New("PVE configuration digest missing, invalid, or changed; reconcile before configuring bootstrap")
		}
		digest = cfg.Digest
	}
	var pools []struct {
		PoolID  string `json:"poolid"`
		Members []struct {
			Type string `json:"type"`
			Node string `json:"node"`
			VMID int    `json:"vmid"`
		} `json:"members"`
	}
	if err := c.request(ctx, http.MethodGet, "/pools?"+url.Values{"poolid": {c.cfg.ResourcePool}, "type": {"lxc"}}.Encode(), nil, &pools); err != nil {
		return "", err
	}
	for _, p := range pools {
		if p.PoolID == c.cfg.ResourcePool {
			for _, m := range p.Members {
				if m.VMID == ref.VMID && m.Node == c.cfg.Node && m.Type == "lxc" {
					return digest, nil
				}
			}
		}
	}
	return "", ErrOwnership
}

func (c *Client) validateOwnedConfig(ref OwnedCT, cfg CTConfig, raw map[string]json.RawMessage) error {
	// PVE's comment serialization adds exactly one trailing LF to descriptions.
	if strings.TrimSuffix(cfg.Description, "\n") != OwnedDescription(ref.Owner) || cfg.Unprivileged != 1 {
		return ErrOwnership
	}
	volume := strings.Split(cfg.RootFS, ",")[0]
	rootPattern := `^` + regexp.QuoteMeta(c.cfg.Storage) + `:(?:subvol|vm)-` + strconv.Itoa(ref.VMID) + `-disk-[0-9]+(?:\.raw)?$`
	if ok, _ := regexp.MatchString(rootPattern, volume); !ok {
		return ErrOwnership
	}
	oci, safe := trustedOCIProfile(raw)
	if !safe {
		return ErrOwnership
	}
	var network string
	if json.Unmarshal(raw["net0"], &network) != nil || !c.ownedNetwork(network, oci) {
		return ErrOwnership
	}
	for k, v := range raw {
		if (strings.HasPrefix(k, "net") && k != "net0") || strings.HasPrefix(k, "mp") || strings.HasPrefix(k, "dev") || strings.HasPrefix(k, "unused") || strings.HasPrefix(k, "lxc.") || k == "hookscript" || (k == "features" && string(v) != `""`) {
			return ErrOwnership
		}
	}
	return nil
}

// trustedOCIProfile permits only the fixed guest image's PVE-generated settings.
// PVE exposes low-level options as [key,value] pairs, not dotted object keys.
func trustedOCIProfile(raw map[string]json.RawMessage) (oci, safe bool) {
	var entrypoint string
	if v, exists := raw["entrypoint"]; exists {
		if json.Unmarshal(v, &entrypoint) != nil || entrypoint != "/usr/bin/tini -- /usr/local/bin/brood-guest" {
			return false, false
		}
		oci = true
		var cmode string
		if json.Unmarshal(raw["cmode"], &cmode) != nil || cmode != "console" {
			return false, false
		}
	}
	var pairs [][]string
	if v, exists := raw["lxc"]; exists {
		if json.Unmarshal(v, &pairs) != nil || pairs == nil {
			return false, false
		}
	}
	seen := make(map[string]bool)
	for _, pair := range pairs {
		if !oci || len(pair) != 2 || seen[pair[0]] {
			return false, false
		}
		seen[pair[0]] = true
		switch pair[0] {
		case "lxc.signal.halt":
			if pair[1] != "SIGTERM" {
				return false, false
			}
		case "lxc.init.cwd":
			if pair[1] != "/workspace" {
				return false, false
			}
		default:
			return false, false
		}
	}
	return oci, !oci || seen["lxc.signal.halt"]
}

func (c *Client) ownedNetwork(raw string, oci bool) bool {
	fields := make(map[string]string)
	for _, part := range strings.Split(raw, ",") {
		key, value, ok := strings.Cut(part, "=")
		if _, exists := fields[key]; !ok || exists {
			return false
		}
		switch key {
		case "name", "bridge", "ip", "hwaddr":
		case "host-managed":
			if !oci || value != "1" {
				return false
			}
		case "type":
			if value != "veth" {
				return false
			}
		case "firewall", "link_down":
			if value != "0" {
				return false
			}
		default:
			return false
		}
		fields[key] = value
	}
	if oci && fields["host-managed"] != "1" {
		return false
	}
	mac, err := net.ParseMAC(fields["hwaddr"])
	return fields["name"] == "eth0" && fields["bridge"] == c.cfg.Network && fields["ip"] == "dhcp" && err == nil && len(mac) == 6 && mac[0]&1 == 0 && !bytes.Equal(mac, make([]byte, 6))
}

// ConfigureBootstrap replaces only the Brood bootstrap environment on a stopped,
// fully owned container. Call after OCI import (which replaces create-time env)
// and after every token rotation, before Start. It never starts or stops a CT.
// PVE's PUT config API is synchronous and returns null, not a UPID. A nil result
// confirms the config write, not guest readiness. No automatic retries occur.
// Missing/changed digests fail closed. Pool membership and stopped status have
// no PVE atomic precondition: operators must not mutate allocations concurrently.
// Errors after submission are sanitized and uncertain, as for Create; callers
// must reconcile before retrying. Bootstrap values never appear in public config.
func (c *Client) ConfigureBootstrap(ctx context.Context, ref OwnedCT, env BootstrapEnv) error {
	encoded, err := c.bootstrapEnv(env)
	if err != nil {
		return err
	}
	if encoded == "" {
		return errors.New("PVE guest bootstrap environment must not be empty")
	}
	digest, err := c.ownedDigest(ctx, ref, true)
	if err != nil {
		return err
	}
	status, err := c.Status(ctx, ref.VMID)
	if err != nil {
		return err
	}
	if status.Status != "stopped" {
		return errors.New("PVE container must be stopped before configuring bootstrap")
	}
	var result json.RawMessage
	err = c.requestData(ctx, http.MethodPut, c.ctPath(ref.VMID)+"/config", url.Values{"env": {encoded}, "digest": {digest}}, &result, true)
	if err != nil {
		if errors.Is(err, ErrPermission) || errors.Is(err, ErrCollision) {
			return err
		}
		return fmt.Errorf("%w: %w", ErrUncertain, err)
	}
	if string(bytes.TrimSpace(result)) != "null" {
		return fmt.Errorf("%w: invalid PVE config update result", ErrUncertain)
	}
	return nil
}

// Start validates latest ownership/configuration and pool membership before power-on.
func (c *Client) Start(ctx context.Context, ref OwnedCT) (string, error) {
	return c.mutate(ctx, ref, "start")
}

// Shutdown requests a graceful shutdown without force-stop fallback.
func (c *Client) Shutdown(ctx context.Context, ref OwnedCT) (string, error) {
	return c.mutate(ctx, ref, "shutdown")
}

// Stop requests PVE stop without force, skiplock, or task-overriding parameters.
func (c *Client) Stop(ctx context.Context, ref OwnedCT) (string, error) {
	return c.mutate(ctx, ref, "stop")
}

// Delete removes only the fully owned allocation, without purge or force.
// PVE power/delete endpoints do not support atomic ownership/digest preconditions.
// A privileged replacement between the live guard and the mutation remains a
// residual race; callers/operators must not replace active allocations out of band.
// ConfigureBootstrap is the only config update and uses PVE's digest CAS.
func (c *Client) Delete(ctx context.Context, ref OwnedCT) (string, error) {
	return c.mutate(ctx, ref, "delete")
}
func (c *Client) mutate(ctx context.Context, ref OwnedCT, op string) (string, error) {
	if err := c.assertOwned(ctx, ref); err != nil {
		return "", err
	}
	path := c.ctPath(ref.VMID)
	method := http.MethodDelete
	if op != "delete" {
		path += "/status/" + op
		method = http.MethodPost
	}
	return c.writeTask(ctx, method, path, url.Values{})
}
func (c *Client) validTask(upid string) bool {
	m := upidPattern.FindStringSubmatch(upid)
	return len(m) > 1 && m[1] == c.cfg.Node
}
func (c *Client) writeTask(ctx context.Context, method, path string, form url.Values) (string, error) {
	var upid string
	err := c.request(ctx, method, path, form, &upid)
	if err != nil {
		if errors.Is(err, ErrPermission) || errors.Is(err, ErrCollision) {
			return "", err
		}
		return "", fmt.Errorf("%w: %w", ErrUncertain, err)
	}
	if !c.validTask(upid) {
		return "", fmt.Errorf("%w: invalid PVE task identifier", ErrUncertain)
	}
	return upid, nil
}

// WaitTask polls the configured node until a task has stopped with exitstatus OK.
// Unknown/missing state, transport failures, and context cancellation are uncertain;
// a failed task never discloses its upstream exit text or assumes rollback.
func (c *Client) WaitTask(ctx context.Context, upid string) error {
	if !c.validTask(upid) {
		return fmt.Errorf("%w: invalid PVE task identifier", ErrUncertain)
	}
	for {
		var state struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		err := c.request(ctx, http.MethodGet, "/nodes/"+c.cfg.Node+"/tasks/"+url.PathEscape(upid)+"/status", nil, &state)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrUncertain, err)
		}
		switch state.Status {
		case "stopped":
			if state.ExitStatus == "OK" {
				return nil
			}
			if state.ExitStatus == "" {
				return fmt.Errorf("%w: missing task exit status", ErrUncertain)
			}
			return ErrTaskFailed
		case "running":
		default:
			return fmt.Errorf("%w: invalid task status", ErrUncertain)
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("%w: %w", ErrUncertain, ctx.Err())
		case <-timer.C:
		}
	}
}

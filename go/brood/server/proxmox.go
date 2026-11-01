package server

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"brood.dev/go/brood/store"
)

const (
	proxmoxPoolsSetting  = "BROOD_PROXMOX_POOLS"
	proxmoxTokensSetting = "BROOD_PROXMOX_TOKENS"
)

// proxmoxRegistry holds installation-local configuration, not native WorkerPool state.
// Credentials and trust material stay out of exported Config fields and API DTOs.
type proxmoxRegistry struct {
	pools                     map[string]proxmoxPool
	tokens                    map[string]string
	publicOrigin, relaySecret string
}

type proxmoxPool struct {
	endpoint, tokenID, node, resourcePool, storage, network, credentialRef, caPEM string
	runtime                                                                       *proxmoxRuntimeConfig
}

type proxmoxRuntimeConfig struct {
	template, digest           string
	actors, diskGiB            int64
	cpuMillicores, memoryBytes int64
}

type proxmoxPoolDTO struct {
	Name                 string           `json:"name"`
	Endpoint             string           `json:"endpoint"`
	TokenID              string           `json:"tokenID"`
	Node                 string           `json:"node"`
	ResourcePool         string           `json:"resourcePool"`
	Storage              string           `json:"storage"`
	Network              string           `json:"network"`
	CredentialConfigured bool             `json:"credentialConfigured"`
	RuntimeAvailable     bool             `json:"runtimeAvailable"`
	ManagedPool          *managedPoolJSON `json:"managedPool,omitempty"`
}

func loadProxmoxRegistry(get getenv) (*proxmoxRegistry, error) {
	poolsJSON, tokensJSON := envGet(get, proxmoxPoolsSetting), envGet(get, proxmoxTokensSetting)
	if poolsJSON == "" && tokensJSON == "" {
		return nil, nil
	}
	if poolsJSON == "" {
		return nil, proxmoxConfigError(proxmoxPoolsSetting, "is required with BROOD_PROXMOX_TOKENS")
	}
	if tokensJSON == "" {
		return nil, proxmoxConfigError(proxmoxTokensSetting, "is required with BROOD_PROXMOX_POOLS")
	}
	pools, err := proxmoxObject(poolsJSON)
	if err != nil {
		return nil, proxmoxConfigError(proxmoxPoolsSetting, "must be a JSON object with unique keys")
	}
	tokens, err := proxmoxObject(tokensJSON)
	if err != nil {
		return nil, proxmoxConfigError(proxmoxTokensSetting, "must be a JSON object with unique keys")
	}
	registry := &proxmoxRegistry{pools: make(map[string]proxmoxPool), tokens: make(map[string]string), publicOrigin: envGet(get, "BROOD_PUBLIC_ORIGIN"), relaySecret: envGet(get, "BROOD_PROXMOX_RELAY_SECRET")}
	if registry.publicOrigin != "" && !proxmoxOrigin(registry.publicOrigin) {
		return nil, proxmoxConfigError("BROOD_PUBLIC_ORIGIN", "must be an HTTPS origin")
	}
	if registry.relaySecret != "" && len(registry.relaySecret) < 32 {
		return nil, proxmoxConfigError("BROOD_PROXMOX_RELAY_SECRET", "must contain at least 32 bytes")
	}
	for ref, raw := range tokens {
		var secret string
		if !proxmoxText(ref) || json.Unmarshal(raw, &secret) != nil || !proxmoxHeaderValue(secret) {
			return nil, proxmoxConfigError(proxmoxTokensSetting, "requires nonempty credential references and header-safe secrets")
		}
		registry.tokens[ref] = secret
	}
	for name, raw := range pools {
		if !proxmoxText(name) {
			return nil, proxmoxConfigError(proxmoxPoolsSetting, "requires nonempty pool names without controls")
		}
		pool, err := parseProxmoxPool(raw)
		if err != nil {
			return nil, err
		}
		if pool.runtime != nil && (len(name) > 48 || !regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`).MatchString(name)) {
			return nil, proxmoxConfigError(proxmoxPoolsSetting, "runtime pool names must be lowercase DNS labels of at most 48 characters")
		}
		if registry.tokens[pool.credentialRef] == "" {
			return nil, proxmoxConfigError(proxmoxTokensSetting, "credentialRef must reference a configured secret")
		}
		registry.pools[name] = pool
	}
	return registry, nil
}

func proxmoxConfigError(setting, detail string) error {
	return &ConfigError{Setting: setting, Detail: detail}
}

// proxmoxObject parses object members without accepting duplicate or escaped duplicate keys.
// Callers replace decoder errors so submitted input never enters diagnostics.
func proxmoxObject(raw string) (map[string]json.RawMessage, error) {
	if !utf8.ValidString(raw) {
		return nil, errors.New("invalid JSON encoding")
	}
	d := json.NewDecoder(strings.NewReader(raw))
	first, err := d.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("expected object")
	}
	fields := make(map[string]json.RawMessage)
	for d.More() {
		token, err := d.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("expected key")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate key")
		}
		var value json.RawMessage
		if err := d.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, errors.New("trailing JSON")
	}
	return fields, nil
}

func parseProxmoxPool(raw json.RawMessage) (proxmoxPool, error) {
	var pool proxmoxPool
	fields, err := proxmoxObject(string(raw))
	if err != nil {
		return pool, proxmoxConfigError(proxmoxPoolsSetting, "pool must be an object with unique fields")
	}
	known := map[string]*string{"endpoint": &pool.endpoint, "tokenID": &pool.tokenID, "node": &pool.node, "resourcePool": &pool.resourcePool, "storage": &pool.storage, "network": &pool.network, "credentialRef": &pool.credentialRef, "caPEM": &pool.caPEM}
	for field := range fields {
		if _, ok := known[field]; !ok && field != "template" && field != "templateDigest" && field != "capacity" {
			return pool, proxmoxConfigError(proxmoxPoolsSetting, "pool contains an unknown field")
		}
	}
	for _, field := range []string{"endpoint", "tokenID", "node", "resourcePool", "storage", "network", "credentialRef", "caPEM"} {
		value := known[field]
		if json.Unmarshal(fields[field], value) != nil || strings.TrimSpace(*value) == "" {
			return pool, proxmoxConfigError(proxmoxPoolsSetting, field+" must be a nonempty string")
		}
		if field != "caPEM" && !proxmoxText(*value) {
			return pool, proxmoxConfigError(proxmoxPoolsSetting, field+" must not contain controls")
		}
	}
	if !proxmoxOrigin(pool.endpoint) {
		return pool, proxmoxConfigError(proxmoxPoolsSetting, "endpoint must be an HTTPS origin with a valid port and no credentials, path, query or fragment")
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+@[A-Za-z0-9_.-]+![A-Za-z0-9_.-]+$`).MatchString(pool.tokenID) {
		return pool, proxmoxConfigError(proxmoxPoolsSetting, "tokenID must have user@realm!token format")
	}
	if !proxmoxCA(pool.caPEM) {
		return pool, proxmoxConfigError(proxmoxPoolsSetting, "caPEM must contain only valid CA certificates in PEM format")
	}
	runtime, err := parseProxmoxRuntime(fields)
	if err != nil {
		return pool, err
	}
	pool.runtime = runtime
	return pool, nil
}

func parseProxmoxRuntime(fields map[string]json.RawMessage) (*proxmoxRuntimeConfig, error) {
	_, template := fields["template"]
	_, digest := fields["templateDigest"]
	_, capacity := fields["capacity"]
	if !template && !digest && !capacity {
		return nil, nil
	}
	bad := func() (*proxmoxRuntimeConfig, error) {
		return nil, proxmoxConfigError(proxmoxPoolsSetting, "runtime requires an explicit vztmpl URI, sha256 templateDigest and positive capacity actors, decimal-string cpuMillicores/memoryBytes and diskGiB")
	}
	if !template || !digest || !capacity {
		return bad()
	}
	r := &proxmoxRuntimeConfig{}
	if json.Unmarshal(fields["template"], &r.template) != nil || !regexp.MustCompile(`^[A-Za-z0-9_-]+:vztmpl/brood-claude-proxmox-[A-Za-z0-9][A-Za-z0-9._-]*$`).MatchString(r.template) {
		return bad()
	}
	if json.Unmarshal(fields["templateDigest"], &r.digest) != nil || !regexp.MustCompile(`^sha256:[a-f0-9]{64}$`).MatchString(r.digest) {
		return bad()
	}
	values, err := proxmoxObject(string(fields["capacity"]))
	if err != nil || len(values) != 4 {
		return bad()
	}
	if json.Unmarshal(values["actors"], &r.actors) != nil || r.actors <= 0 || json.Unmarshal(values["diskGiB"], &r.diskGiB) != nil || r.diskGiB <= 0 {
		return bad()
	}
	for key, target := range map[string]*int64{"cpuMillicores": &r.cpuMillicores, "memoryBytes": &r.memoryBytes} {
		var text string
		if json.Unmarshal(values[key], &text) != nil || !regexp.MustCompile(`^[1-9][0-9]*$`).MatchString(text) {
			return bad()
		}
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return bad()
		}
		*target = n
	}
	if r.diskGiB > int64(^uint64(0)>>1)/gib || strings.Contains(r.template, "..") {
		return bad()
	}
	if _, err := managedActorResources(store.ManagedPool{MaxActors: r.actors, Resources: store.ManagedResources{CPUMillicores: r.cpuMillicores, MemoryBytes: r.memoryBytes, DiskBytes: r.diskGiB * gib}}); err != nil {
		return bad()
	}
	return r, nil
}

func proxmoxText(value string) bool {
	return strings.TrimSpace(value) != "" && strings.IndexFunc(value, unicode.IsControl) < 0
}

func proxmoxHeaderValue(value string) bool {
	if !proxmoxText(value) {
		return false
	}
	for _, r := range value {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func proxmoxOrigin(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawPath != "" || strings.ContainsAny(value, "?#") {
		return false
	}
	// Парсер URL отвергает нечисловой порт, но допускает пустой и выход за диапазон.
	if strings.HasSuffix(u.Host, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	return true
}

func proxmoxCA(value string) bool {
	count := 0
	for strings.TrimSpace(value) != "" {
		value = strings.TrimSpace(value)
		if !strings.HasPrefix(value, "-----BEGIN CERTIFICATE-----") {
			return false
		}
		block, rest := pem.Decode([]byte(value))
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return false
		}
		// pem.Decode может пропустить повреждённый блок перед валидным.
		consumed := value[:len(value)-len(rest)]
		if strings.Count(consumed, "-----BEGIN") != 1 {
			return false
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA || !cert.BasicConstraintsValid {
			return false
		}
		count++
		value = string(rest)
	}
	return count > 0
}

func (registry *proxmoxRegistry) items() []proxmoxPoolDTO {
	items := make([]proxmoxPoolDTO, 0)
	if registry == nil {
		return items
	}
	names := make([]string, 0, len(registry.pools))
	for name := range registry.pools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pool := registry.pools[name]
		items = append(items, proxmoxPoolDTO{Name: name, Endpoint: pool.endpoint, TokenID: pool.tokenID, Node: pool.node, ResourcePool: pool.resourcePool, Storage: pool.storage, Network: pool.network, CredentialConfigured: registry.tokens[pool.credentialRef] != "", RuntimeAvailable: pool.runtime != nil && registry.publicOrigin != "" && len(registry.relaySecret) >= 32})
	}
	return items
}

func (s *Server) mountProxmoxPools(registry *proxmoxRegistry) {
	s.mux.HandleFunc("GET /api/proxmox-pools", func(w http.ResponseWriter, r *http.Request) {
		user, ok := currentUser(w, r)
		if !ok {
			return
		}
		if !user.IsAdmin {
			writeDetail(w, http.StatusForbidden, "administrator access required")
			return
		}
		items, err := s.managed.inventory(r.Context())
		if err != nil {
			apiError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	})
	for _, action := range []string{"activate", "drain", "enable"} {
		s.mux.HandleFunc("POST /api/proxmox-pools/{name}/"+action, func(w http.ResponseWriter, r *http.Request) {
			r.SetPathValue("action", action)
			s.handleManagedPool(w, r)
		})
	}
	s.mux.HandleFunc("DELETE /api/proxmox-pools/{name}", func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("action", "delete")
		s.handleManagedPool(w, r)
	})
}

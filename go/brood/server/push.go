package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"brood.dev/go/brood/auth"
)

const (
	pushChunkLimit  int64 = 32 << 20
	pushBlobLimit   int64 = 8 << 30
	pushUploadLimit       = 4096
	pushUploadTTL         = 30 * time.Minute
)

var pushLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var pushSHA = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var pushHost = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?|\[[a-fA-F0-9:]+\])(?::[0-9]{1,5})?$`)

func (cfg Config) validateRegistry() error {
	if cfg.RegistryURL == "" && cfg.RegistryImageHost == "" {
		return nil
	}
	if cfg.RegistryURL == "" || cfg.RegistryImageHost == "" {
		return &ConfigError{Setting: "BROOD_REGISTRY_URL", Detail: "and BROOD_REGISTRY_IMAGE_HOST must be set together"}
	}
	u, err := url.Parse(cfg.RegistryURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !validPushAuthority(u.Host) || u.User != nil || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return &ConfigError{Setting: "BROOD_REGISTRY_URL", Detail: "must be an http(s) origin without credentials, path, query or fragment"}
	}
	if !validPushAuthority(cfg.RegistryImageHost) {
		return &ConfigError{Setting: "BROOD_REGISTRY_IMAGE_HOST", Detail: "must be a registry host with optional port"}
	}
	return nil
}

func validPushAuthority(authority string) bool {
	if !pushHost.MatchString(authority) || len(authority) > 253 {
		return false
	}
	u, err := url.Parse("http://" + authority)
	if err != nil || strings.HasSuffix(authority, ":") {
		return false
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return false
		}
	}
	if strings.HasPrefix(authority, "[") {
		return net.ParseIP(u.Hostname()) != nil
	}
	for _, label := range strings.Split(strings.ToLower(u.Hostname()), ".") {
		if !pushLabel.MatchString(label) {
			return false
		}
	}
	return true
}

type pushUpload struct {
	mu                   sync.Mutex
	session              [32]byte
	user, repo, location string
	expires              time.Time
	size                 int64
}
type pushRegistry struct {
	origin    *url.URL
	imageHost string
	client    *http.Client
	mu        sync.Mutex
	uploads   map[string]*pushUpload
	starting  int
}

func (s *Server) mountPush(cfg Config) {
	if cfg.RegistryURL != "" {
		origin, _ := url.Parse(cfg.RegistryURL)
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.Proxy = nil
		s.push = &pushRegistry{origin: origin, imageHost: cfg.RegistryImageHost, uploads: make(map[string]*pushUpload), client: &http.Client{Transport: transport, Timeout: 90 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	}
	base := "/api/swarms/{id}/builds/{environment}"
	for _, route := range []string{"HEAD " + base + "/blobs/{digest}", "POST " + base + "/uploads", "PATCH " + base + "/uploads/{upload}", "PUT " + base + "/uploads/{upload}", "PUT " + base + "/manifests/{digest}", "POST " + base + "/templates"} {
		s.mux.HandleFunc(route, s.handlePush)
	}
}

func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	user, ok := currentUser(w, r)
	if !ok {
		return
	}
	sw, ok := s.swarmFor(w, r, user)
	if !ok {
		return
	}
	env := r.PathValue("environment")
	if !pushLabel.MatchString(env) {
		writeDetail(w, 400, "environment must be a lower-case label")
		return
	}
	if s.push == nil {
		writeDetail(w, 503, "push is not configured: BROOD_REGISTRY_URL and BROOD_REGISTRY_IMAGE_HOST are required")
		return
	}
	repo := sw.AtespaceName + "/" + env
	switch {
	case r.Method == "HEAD":
		digest := r.PathValue("digest")
		if !pushSHA.MatchString(digest) {
			writeDetail(w, 400, "invalid sha256 digest")
			return
		}
		status := s.push.head(r, repo, "blobs", digest)
		if status == 200 || status == 404 {
			w.WriteHeader(status)
		} else {
			writeDetail(w, 502, "registry request failed")
		}
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/uploads"):
		s.pushStart(w, r, repo, user.ID)
	case r.PathValue("upload") != "":
		s.pushContinue(w, r, repo, user.ID)
	case r.Method == "PUT":
		s.pushManifest(w, r, repo)
	case r.Method == "POST":
		s.pushTemplate(w, r, sw, repo)
	}
}

func (p *pushRegistry) request(r *http.Request, method, target string, body io.Reader, length int64, media string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(r.Context(), method, target, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = length
	if media != "" {
		req.Header.Set("Content-Type", media)
	}
	if strings.Contains(target, "/manifests/") {
		req.Header.Set("Accept", pushManifestType+", "+pushIndexType)
	}
	return p.client.Do(req)
}
func (p *pushRegistry) target(repo, path string) string {
	return p.origin.String() + "/v2/" + repo + "/" + path
}
func (p *pushRegistry) head(r *http.Request, repo, kind, digest string) int {
	resp, err := p.request(r, "HEAD", p.target(repo, kind+"/"+digest), nil, 0, "")
	if err != nil {
		return 502
	}
	defer resp.Body.Close()
	return resp.StatusCode
}
func (p *pushRegistry) uploadLocation(base, location, repo string) (string, error) {
	if location == "" || len(location) > 8192 {
		return "", errors.New("invalid registry upload location")
	}
	reference, err := url.Parse(location)
	if err != nil {
		return "", err
	}
	root, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	u := root.ResolveReference(reference)
	prefix := "/v2/" + repo + "/blobs/uploads/"
	id := strings.TrimPrefix(u.Path, prefix)
	if u.Scheme != p.origin.Scheme || u.Host != p.origin.Host || u.User != nil || u.Fragment != "" || u.RawPath != "" || !strings.HasPrefix(u.Path, prefix) || id == "" || strings.ContainsAny(id, "/\\") || id == "." || id == ".." {
		return "", errors.New("invalid registry upload location")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", err
	}
	for key := range query {
		if key != "_state" {
			return "", errors.New("invalid registry upload query")
		}
	}
	return u.String(), nil
}
func pushSession(r *http.Request) [32]byte {
	cookie, err := r.Cookie(auth.CookieSession)
	if err != nil {
		return [32]byte{}
	}
	return sha256.Sum256([]byte(cookie.Value))
}
func (s *Server) pushStart(w http.ResponseWriter, r *http.Request, repo, user string) {
	p := s.push
	p.mu.Lock()
	now := time.Now()
	for id, u := range p.uploads {
		if !now.Before(u.expires) {
			delete(p.uploads, id)
		}
	}
	if len(p.uploads)+p.starting >= pushUploadLimit {
		p.mu.Unlock()
		writeDetail(w, 503, "upload capacity exhausted")
		return
	}
	p.starting++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.starting--
		p.mu.Unlock()
	}()
	target := p.target(repo, "blobs/uploads/")
	resp, err := p.request(r, "POST", target, nil, 0, "")
	if err != nil {
		writeDetail(w, 502, "registry request failed")
		return
	}
	defer resp.Body.Close()
	location, err := p.uploadLocation(target, resp.Header.Get("Location"), repo)
	if resp.StatusCode != 202 || err != nil {
		writeDetail(w, 502, "registry upload rejected")
		return
	}
	token := make([]byte, 32)
	if _, err := rand.Read(token); err != nil {
		writeDetail(w, 500, "cannot create upload")
		return
	}
	id := hex.EncodeToString(token)
	p.mu.Lock()
	p.uploads[id] = &pushUpload{session: pushSession(r), user: user, repo: repo, location: location, expires: now.Add(pushUploadTTL)}
	p.mu.Unlock()
	writeJSON(w, 201, map[string]string{"upload_id": id})
}
func (s *Server) pushContinue(w http.ResponseWriter, r *http.Request, repo, user string) {
	p := s.push
	id := r.PathValue("upload")
	p.mu.Lock()
	u := p.uploads[id]
	p.mu.Unlock()
	if u == nil {
		writeDetail(w, 404, "upload not found")
		return
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.session != pushSession(r) || u.user != user || u.repo != repo || !time.Now().Before(u.expires) || u.location == "" {
		writeDetail(w, 404, "upload not found")
		return
	}
	drop := func() { p.mu.Lock(); delete(p.uploads, id); p.mu.Unlock(); u.location = "" }
	target := u.location
	var body io.Reader
	var length int64
	media := ""
	if r.Method == "PATCH" {
		if r.ContentLength < 0 {
			writeDetail(w, 411, "Content-Length is required")
			return
		}
		if r.ContentLength > pushChunkLimit || u.size+r.ContentLength > pushBlobLimit {
			writeDetail(w, 413, "upload size limit exceeded")
			return
		}
		typ, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || typ != "application/octet-stream" {
			writeDetail(w, 415, "application/octet-stream is required")
			return
		}
		body = http.MaxBytesReader(w, r.Body, pushChunkLimit)
		length = r.ContentLength
		media = "application/octet-stream"
	} else {
		digest := r.URL.Query().Get("digest")
		if !pushSHA.MatchString(digest) {
			writeDetail(w, 400, "invalid sha256 digest")
			return
		}
		parsed, _ := url.Parse(target)
		q := parsed.Query()
		q.Set("digest", digest)
		parsed.RawQuery = q.Encode()
		target = parsed.String()
	}
	resp, err := p.request(r, r.Method, target, body, length, media)
	if err != nil {
		drop()
		writeDetail(w, 502, "registry request failed")
		return
	}
	defer resp.Body.Close()
	if r.Method == "PUT" {
		if resp.StatusCode != 201 {
			writeDetail(w, 502, "registry finalization rejected")
			return
		}
		drop()
		w.WriteHeader(204)
		return
	}
	location, err := p.uploadLocation(target, resp.Header.Get("Location"), repo)
	if resp.StatusCode != 202 || err != nil {
		drop()
		writeDetail(w, 502, "registry upload rejected")
		return
	}
	u.location = location
	u.size += length
	w.WriteHeader(204)
}

func pushContentDigest(data []byte) string { return fmt.Sprintf("sha256:%x", sha256.Sum256(data)) }

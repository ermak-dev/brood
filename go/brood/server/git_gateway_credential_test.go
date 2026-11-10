package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/gitcredential"
)

const gitRotatedToken = "synthetic-rotated-git-token"

// projectCredentialGateway is gitGatewaySetup whose gateway access comes only
// from a project Git credential: the installation has no remotes at all.
func projectCredentialGateway(t *testing.T) gitGatewayFixture {
	t.Helper()
	return gitGatewaySetupWith(t, func(ts *substrateServer, project string) {
		ts.srv.git.remotes = gitRemotes{}
		putProjectGitCredential(t, ts, project, "gitlab", gitTestOrigin, gitTokenSentinel)
		if err := ts.srv.store.SetProjectGitCredentialRef(context.Background(), project, "gitlab"); err != nil {
			t.Fatal(err)
		}
	})
}

func putProjectGitCredential(t *testing.T, ts *substrateServer, project, name, repository, token string) {
	t.Helper()
	c := gitcredential.Credential{Name: name, Repository: repository, Username: "brood-bot", Token: token}
	if err := ts.srv.store.PutProjectGitCredential(context.Background(), project, c); err != nil {
		t.Fatal(err)
	}
}

func (f gitGatewayFixture) guestGet(t *testing.T, path string) (int, string) {
	t.Helper()
	w := f.serve("GET", path, nil, bearer(f.token))
	return w.Code, w.Body.String()
}

func (f gitGatewayFixture) actorMode(t *testing.T, name string) string {
	t.Helper()
	r, b, _ := f.ts.do(f.alice, "GET", "/api/swarms/"+f.swarm+"/actors/"+name, "", nil)
	expectStatus(t, "read actor", r, b, http.StatusOK)
	mode, _ := b["originMode"].(string)
	return mode
}

// FEATURE: Gateway access from a project Git credential (#173).
// SPEC: With no installation remote, a project credential referenced by the
// declaration and bound to exactly the project repository gives the actor
// origin gateway mode at creation, on read and on every gateway request; the
// gateway sends that credential and only it, preferring it over a legacy
// remote. Clearing the reference, referencing a credential bound elsewhere or
// removing the credential revokes the mode. Guest-facing endpoints never
// carry the credential.
// IMPLEMENTATION: One access snapshot feeds origin mode and upstream credential.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestGitGatewayProjectCredential(t *testing.T) {
	f := projectCredentialGateway(t)
	ctx := context.Background()
	refs := f.base() + "/info/refs?service=git-upload-pack"
	if mode := f.actorMode(t, "clone"); mode != "gateway" {
		t.Fatalf("actor origin mode = %q", mode)
	}
	if code, body := f.guestGet(t, refs); code != http.StatusOK || body != "advertisement" {
		t.Fatalf("project credential request: %d %s", code, body)
	}
	for _, path := range []string{
		fmt.Sprintf("/api/proxmox/workspace/%s/%d", f.actor.UID, f.actor.Incarnation),
		fmt.Sprintf("/api/proxmox/secrets/%s/%d", f.actor.UID, f.actor.Incarnation),
	} {
		code, body := f.guestGet(t, path)
		if code != http.StatusOK || strings.Contains(body, gitTokenSentinel) || strings.Contains(body, "brood-bot") {
			t.Fatalf("guest endpoint %s: %d %s", path, code, body)
		}
	}
	// Panel and client responses about the bound access carry no value either.
	admin := newClient(t)
	f.ts.login(admin, "root", "root-password-1")
	for _, client := range []*http.Client{f.alice, admin} {
		for _, path := range []string{"/api/swarms/" + f.swarm + "/actors", "/api/swarms/" + f.swarm + "/actors/clone"} {
			r, b, raw := f.ts.do(client, "GET", path, "", nil)
			expectStatus(t, "read "+path, r, b, http.StatusOK)
			if !strings.Contains(raw, `"originMode":"gateway"`) || strings.Contains(raw, gitTokenSentinel) || strings.Contains(raw, "brood-bot") {
				t.Fatalf("actor response %s: %s", path, raw)
			}
		}
	}
	declaration := map[string]any{"name": "proj", "repository": gitTestOrigin, "git_credential": "gitlab"}
	r, b, raw := f.ts.do(admin, "PUT", "/api/projects/sync", f.ts.csrf(admin), declaration)
	expectStatus(t, "sync", r, b, http.StatusOK)
	if b["git_credential_bound"] != true || b["origin_mode"] != "gateway" || strings.Contains(raw, gitTokenSentinel) || strings.Contains(raw, "brood-bot") {
		t.Fatalf("sync response: %s", raw)
	}
	// A legacy remote for the same repository keeps working but yields to the project credential.
	f.ts.srv.git.remotes[gitTestOrigin] = gitCredential{username: "legacy-bot", token: "synthetic-legacy-token"}
	if code, body := f.guestGet(t, refs); code != http.StatusOK {
		t.Fatalf("project credential over legacy remote: %d %s", code, body)
	}
	delete(f.ts.srv.git.remotes, gitTestOrigin)
	for _, request := range f.upstream.seen() {
		user, password, ok := (&http.Request{Header: request.header}).BasicAuth()
		if !ok || user != "brood-bot" || password != gitTokenSentinel {
			t.Fatalf("upstream did not receive the project credential: %s %s", request.method, request.path)
		}
	}
	seen := len(f.upstream.seen())
	if seen != 2 {
		t.Fatalf("upstream requests = %d", seen)
	}

	for _, tc := range []struct {
		name  string
		apply func()
	}{
		{"cleared reference", func() {
			if err := f.ts.srv.store.SetProjectGitCredentialRef(ctx, f.project, ""); err != nil {
				t.Fatal(err)
			}
		}},
		{"credential bound to another repository", func() {
			putProjectGitCredential(t, f.ts, f.project, "other", "https://git.example.com/group/other.git", gitTokenSentinel)
			if err := f.ts.srv.store.SetProjectGitCredentialRef(ctx, f.project, "other"); err != nil {
				t.Fatal(err)
			}
		}},
		{"removed credential", func() {
			if err := f.ts.srv.store.SetProjectGitCredentialRef(ctx, f.project, "gitlab"); err != nil {
				t.Fatal(err)
			}
			if err := f.ts.srv.store.RemoveProjectGitCredentials(ctx, f.project, []string{"gitlab"}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		tc.apply()
		if code, _ := f.guestGet(t, refs); code != http.StatusForbidden {
			t.Fatalf("%s: gateway status %d", tc.name, code)
		}
		if mode := f.actorMode(t, "clone"); mode != "" {
			t.Fatalf("%s: actor origin mode = %q", tc.name, mode)
		}
		r, _, raw := f.ts.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/actors", f.ts.csrf(f.alice), managedOriginBody("refused", gitTestOrigin))
		if r.StatusCode != http.StatusUnprocessableEntity || strings.Contains(raw, gitTokenSentinel) {
			t.Fatalf("%s: actor creation %d %s", tc.name, r.StatusCode, raw)
		}
	}
	if len(f.upstream.seen()) != seen {
		t.Fatal("revoked requests reached upstream")
	}
}

// blockingUpstream makes every authenticated upstream request send "first"
// and hold the stream until the gateway cancels it. open starts a guest stream
// through the real listener and returns once "first" arrived; canceled
// receives one value per upstream request the gateway canceled.
func (f gitGatewayFixture) blockingUpstream(t *testing.T) (open func() *http.Response, canceled <-chan struct{}) {
	t.Helper()
	blocked := make(chan struct{})
	// Cleanup runs in reverse order: handlers are released before the upstream closes.
	t.Cleanup(func() { close(blocked) })
	cancels := make(chan struct{}, 8)
	f.upstream.setInner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		_, _ = io.WriteString(w, "first")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
			cancels <- struct{}{}
		case <-blocked:
		}
	}))
	refs := f.ts.http.URL + f.base() + "/info/refs?service=git-upload-pack"
	open = func() *http.Response {
		t.Helper()
		req, _ := http.NewRequest("GET", refs, nil)
		req.Header.Set("Authorization", "Bearer "+f.token)
		response, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		first := make([]byte, 5)
		if _, err := io.ReadFull(response.Body, first); err != nil || string(first) != "first" {
			t.Fatalf("stream did not start: %d %q %v", response.StatusCode, first, err)
		}
		return response
	}
	return open, cancels
}

// awaitCanceled waits for the gateway to cancel the open upstream stream.
func awaitCanceled(t *testing.T, canceled <-chan struct{}, response *http.Response, what string) {
	t.Helper()
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatalf("open stream survived %s", what)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
}

// SPEC: Rotation and removal of the project credential, clearing the
// declaration reference (git_credential "") and a repository change through
// sync cancel an open stream within a second; every request after rotation
// carries the rotated credential, and sync responses never carry a value.
func TestGitGatewayProjectCredentialRevokesOpenStreams(t *testing.T) {
	f := projectCredentialGateway(t)
	ctx := context.Background()
	admin := newClient(t)
	f.ts.login(admin, "root", "root-password-1")
	sync := func(repository, reference string) {
		t.Helper()
		declaration := map[string]any{"name": "proj", "repository": repository, "git_credential": reference}
		r, b, raw := f.ts.do(admin, "PUT", "/api/projects/sync", f.ts.csrf(admin), declaration)
		expectStatus(t, "sync", r, b, http.StatusOK)
		if b["changed"] != true || strings.Contains(raw, gitTokenSentinel) || strings.Contains(raw, gitRotatedToken) || strings.Contains(raw, "brood-bot") {
			t.Fatalf("sync %s %q: %s", repository, reference, raw)
		}
	}
	open, canceled := f.blockingUpstream(t)
	for _, tc := range []struct {
		name            string
		revoke, restore func()
	}{
		{"credential rotation", func() {
			putProjectGitCredential(t, f.ts, f.project, "gitlab", gitTestOrigin, gitRotatedToken)
			f.upstream.rotate(gitRotatedToken)
		}, nil},
		{"cleared declaration reference", func() { sync(gitTestOrigin, "") }, func() { sync(gitTestOrigin, "gitlab") }},
		{"project repository change", func() { sync("https://elsewhere.example/group/repo.git", "gitlab") }, func() { sync(gitTestOrigin, "gitlab") }},
		{"credential removal", func() {
			if err := f.ts.srv.store.RemoveProjectGitCredentials(ctx, f.project, []string{"gitlab"}); err != nil {
				t.Fatal(err)
			}
		}, nil},
	} {
		response := open()
		tc.revoke()
		awaitCanceled(t, canceled, response, tc.name)
		if tc.restore != nil {
			tc.restore()
		}
	}
	seen := f.upstream.seen()
	if len(seen) != 4 {
		t.Fatalf("upstream requests = %d", len(seen))
	}
	for _, request := range seen[1:] {
		if _, password, _ := (&http.Request{Header: request.header}).BasicAuth(); password != gitRotatedToken {
			t.Fatal("request after rotation did not carry the rotated credential")
		}
	}
}

// SPEC: Removing the project credential does not revoke a legacy
// BROOD_GIT_REMOTES credential for the same repository: the open stream with
// the removed credential is canceled, the actor keeps gateway mode and the
// next request carries the legacy credential.
func TestGitGatewayProjectCredentialRemovalKeepsLegacyRemote(t *testing.T) {
	const legacyToken = "synthetic-legacy-git-token"
	f := gitGatewaySetupWith(t, func(ts *substrateServer, project string) {
		ts.srv.git.remotes = gitRemotes{gitTestOrigin: {username: "brood-bot", token: legacyToken}}
		putProjectGitCredential(t, ts, project, "gitlab", gitTestOrigin, gitTokenSentinel)
		if err := ts.srv.store.SetProjectGitCredentialRef(context.Background(), project, "gitlab"); err != nil {
			t.Fatal(err)
		}
	})
	open, canceled := f.blockingUpstream(t)
	response := open()
	if err := f.ts.srv.store.RemoveProjectGitCredentials(context.Background(), f.project, []string{"gitlab"}); err != nil {
		t.Fatal(err)
	}
	f.upstream.rotate(legacyToken)
	awaitCanceled(t, canceled, response, "credential removal")
	if mode := f.actorMode(t, "clone"); mode != "gateway" {
		t.Fatalf("actor origin mode after removal = %q", mode)
	}
	response = open()
	response.Body.Close()
	seen := f.upstream.seen()
	if len(seen) != 2 {
		t.Fatalf("upstream requests = %d", len(seen))
	}
	for i, token := range []string{gitTokenSentinel, legacyToken} {
		if _, password, _ := (&http.Request{Header: seen[i].header}).BasicAuth(); password != token {
			t.Fatalf("request %d did not carry the expected credential", i)
		}
	}
}

// FEATURE: SSRF bound of the Git gateway (#173).
// SPEC: Upstreams configured through the API never reach loopback,
// link-local (including cloud metadata), unspecified, multicast, broadcast
// or reserved addresses, even through DNS; private LAN addresses stay
// allowed, as the deploy check allows them for installation remotes.
// IMPLEMENTATION: Dialer control after name resolution.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestGitGatewayDialRefusesUnsafeAddresses(t *testing.T) {
	for _, address := range []string{"127.0.0.1:443", "127.1.2.3:443", "[::1]:443", "169.254.169.254:443", "[fe80::1]:443", "0.0.0.0:443", "0.1.2.3:443", "[::]:443", "224.0.0.1:443", "[ff02::1]:443", "255.255.255.255:443", "240.0.0.1:443", "[::ffff:127.0.0.1]:443", "[::ffff:169.254.169.254]:443", "not-an-address"} {
		if err := gitDialControl("tcp", address, nil); !errors.Is(err, errGitAddressDenied) {
			t.Fatalf("%s: %v", address, err)
		}
	}
	for _, address := range []string{"10.0.0.5:443", "172.16.0.1:443", "192.168.1.66:443", "100.64.0.1:443", "203.0.113.10:443", "[2001:db8::1]:443", "[fd00::1]:443", "[::ffff:10.0.0.5]:443"} {
		if err := gitDialControl("tcp", address, nil); err != nil {
			t.Fatalf("%s: %v", address, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := newGitGateway(nil).transport.DialContext(ctx, "tcp", "127.0.0.1:1"); !errors.Is(err, errGitAddressDenied) {
		t.Fatalf("default gateway dialer reached loopback: %v", err)
	}
}

// SPEC: IPv6 addresses that carry an IPv4 destination for a translator or a
// tunnel are refused whatever IPv4 they embed: NAT64 (64:ff9b::/96 and the
// local-use 64:ff9b:1::/48), 6to4 (2002::/16), Teredo (2001::/32),
// IPv4-compatible (::/96) and IPv4-translated (::ffff:0:0:0/96). Through
// the gateway they could reach loopback or cloud metadata. Neighboring global
// and unique local addresses stay allowed.
func TestGitGatewayDialRefusesTranslatedIPv6(t *testing.T) {
	for _, address := range []string{
		"[64:ff9b::7f00:1]:443", "[64:ff9b::a9fe:a9fe]:443", "[64:ff9b::cb00:710a]:443",
		"[64:ff9b:1::7f00:1]:443", "[64:ff9b:1:ffff:ffff:ffff:ffff:ffff]:443",
		"[2002:7f00:1::1]:443", "[2002:a9fe:a9fe::1]:443", "[2002:cb00:710a::1]:443",
		"[2001:0:4136:e378:8000:63bf:80ff:fffe]:443", "[2001::7f00:1]:443",
		"[::7f00:1]:443", "[::a9fe:a9fe]:443", "[::cb00:710a]:443",
		"[::ffff:0:7f00:1]:443", "[::ffff:0:a9fe:a9fe]:443",
	} {
		if err := gitDialControl("tcp", address, nil); !errors.Is(err, errGitAddressDenied) {
			t.Errorf("%s: %v", address, err)
		}
	}
	for _, address := range []string{"[64:ff9a:ffff:ffff:ffff:ffff:ffff:ffff]:443", "[2003::1]:443", "[2001:1::1]:443", "[2001:db8::7f00:1]:443", "[2606:4700::1111]:443", "[fd00::7f00:1]:443"} {
		if err := gitDialControl("tcp", address, nil); err != nil {
			t.Errorf("%s: %v", address, err)
		}
	}
}

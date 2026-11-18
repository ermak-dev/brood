// Package relay multiplexes terminal streams over an authenticated outbound guest connection.
package relay

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
)

// MaxStreams is the per-incarnation terminal stream limit on both peers.
const MaxStreams = 32
const handshakeTimeout = 10 * time.Second
const revocationInterval = time.Second
const terminalChannel = "brood-terminal"

var errUnavailable = errors.New("guest relay unavailable")

// Hub owns authenticated guest connections. The callback must consult current
// durable actor state, including deletion, generation and token revocation.
// It must be concurrency-safe and bounded; it must not call back into the Hub.
// Connected is transport readiness only: probe ttyd HTTP before marking RUNNING.
type Hub struct {
	mu        sync.Mutex
	signer    ssh.Signer
	authorize func(string, int64, string) bool
	// router опционален: nil — гость не может слать сообщения соседям.
	router  MessageRouter
	swarm   SwarmHandler
	peers   map[identity]*peer
	pending map[*socket]struct{}
	slots   chan struct{}
	closed  bool
}
type identity struct {
	uid        string
	generation int64
}
type peer struct {
	id               identity
	token            string
	conn             ssh.Conn
	ctx              context.Context
	streams          int
	messageJournalID string
}

// NewHub derives a stable Ed25519 SSH host key from a private installation key
// of at least 32 bytes. The key is not a guest authentication token.
func NewHub(installationRelayKey []byte, authorize func(string, int64, string) bool) (*Hub, error) {
	if len(installationRelayKey) < 32 || authorize == nil {
		return nil, errors.New("relay requires an installation key and authorizer")
	}
	mac := hmac.New(sha256.New, installationRelayKey)
	mac.Write([]byte("brood/proxmox/relay/ssh-host/v1"))
	signer, err := ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(mac.Sum(nil)))
	if err != nil {
		return nil, errors.New("relay signer unavailable")
	}
	return &Hub{signer: signer, authorize: authorize, peers: make(map[identity]*peer), pending: make(map[*socket]struct{}), slots: make(chan struct{}, 64)}, nil
}
func (*Hub) String() string     { return "relay.Hub{redacted}" }
func (h *Hub) GoString() string { return h.String() }

// Format serves every verb: String and GoString cover only some, and the rest
// would print the fields. %p bypasses Format and prints an address only.
func (h *Hub) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, h.String()) }

// HostKey returns the public SSH host key in authorized_keys format for guest pinning.
func (h *Hub) HostKey() string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(h.signer.PublicKey())))
}

func validUID(uid string) bool {
	if len(uid) == 0 || len(uid) > 128 {
		return false
	}
	for _, c := range uid {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}
func validToken(s string) bool {
	if len(s) == 0 || len(s) > 4096 {
		return false
	}
	for _, c := range s {
		if c <= 32 || c >= 127 {
			return false
		}
	}
	return true
}
func route(id identity) string {
	return "/api/proxmox/guest/" + id.uid + "/" + strconv.FormatInt(id.generation, 10)
}
func parseRoute(r *http.Request) (identity, bool) {
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) != 6 || parts[1] != "api" || parts[2] != "proxmox" || parts[3] != "guest" || r.URL.RawQuery != "" || r.URL.RawPath != "" {
		return identity{}, false
	}
	g, err := strconv.ParseInt(parts[5], 10, 64)
	id := identity{parts[4], g}
	return id, err == nil && g > 0 && validUID(id.uid) && r.URL.Path == route(id)
}

// Handler exposes only GET /api/proxmox/guest/<actorUID>/<generation>.
// Mount outside browser-cookie middleware: only a machine Bearer token is accepted.
func (h *Hub) Handler() http.Handler { return http.HandlerFunc(h.serveHTTP) }
func (h *Hub) serveHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := parseRoute(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	auth := r.Header.Values("Authorization")
	token := ""
	if len(auth) == 1 && strings.HasPrefix(auth[0], "Bearer ") {
		token = strings.TrimPrefix(auth[0], "Bearer ")
	}
	if !validToken(token) || !h.authorize(id.uid, id.generation, token) {
		http.Error(w, "guest authorization denied", http.StatusUnauthorized)
		return
	}
	messageJournalID, err := messageDedupAdvertisement(r.Header)
	if err != nil {
		http.Error(w, "invalid message delivery protocol advertisement", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	closed := h.closed
	h.mu.Unlock()
	if closed {
		http.Error(w, "relay closed", http.StatusServiceUnavailable)
		return
	}
	select {
	case h.slots <- struct{}{}:
		defer func() { <-h.slots }()
	default:
		http.Error(w, "relay busy", http.StatusServiceUnavailable)
		return
	}
	websocket.Server{Handshake: func(*websocket.Config, *http.Request) error { return nil }, Handler: func(ws *websocket.Conn) { h.accept(newSocket(ws), id, token, messageJournalID) }}.ServeHTTP(w, r)
}
func (h *Hub) accept(ws *socket, id identity, token string, messageJournalID string) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		ws.Close()
		return
	}
	h.pending[ws] = struct{}{}
	h.mu.Unlock()
	defer func() { h.mu.Lock(); delete(h.pending, ws); h.mu.Unlock(); ws.Close() }()
	ws.SetDeadline(time.Now().Add(handshakeTimeout))
	cfg := &ssh.ServerConfig{MaxAuthTries: 1, PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
		user := id.uid + "/" + strconv.FormatInt(id.generation, 10)
		if meta.User() != user || subtle.ConstantTimeCompare(password, []byte(token)) != 1 || !h.authorize(id.uid, id.generation, string(password)) {
			return nil, errors.New("guest authorization denied")
		}
		return nil, nil
	}}
	cfg.AddHostKey(h.signer)
	conn, channels, requests, err := ssh.NewServerConn(ws, cfg)
	if err != nil {
		return
	}
	ws.SetDeadline(time.Time{})
	peerCtx, cancelPeer := context.WithCancel(context.Background())
	defer cancelPeer()
	p := &peer{id: id, token: token, conn: conn, ctx: peerCtx, messageJournalID: messageJournalID}
	h.mu.Lock()
	if h.closed || !h.authorize(id.uid, id.generation, token) {
		h.mu.Unlock()
		conn.Close()
		return
	}
	old := h.peers[id]
	h.peers[id] = p
	delete(h.pending, ws)
	h.mu.Unlock()
	if old != nil {
		old.conn.Close()
	}
	defer func() {
		conn.Close()
		h.mu.Lock()
		if h.peers[id] == p {
			delete(h.peers, id)
		}
		h.mu.Unlock()
	}()
	go h.serveChannels(p, channels)
	go h.serveSwarmRequests(p, requests)
	done := make(chan struct{})
	go func() { conn.Wait(); cancelPeer(); close(done) }()
	ticker := time.NewTicker(revocationInterval)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if !h.authorize(id.uid, id.generation, token) {
				return
			}
		}
	}
}

// SetMessageRouter включает канал сообщений роя. Без роутера входящие
// каналы гостя отвергаются, как прежде; вызывать до первого гостя.
func (h *Hub) SetMessageRouter(router MessageRouter) {
	h.mu.Lock()
	h.router = router
	h.mu.Unlock()
}

// serveChannels разбирает входящие каналы гостя: сообщения роя идут
// в роутер, остальное отвергается. Число одновременно обрабатываемых
// сообщений на гостя ограничено: каждый канал держит горутину и запросы
// к хранилищу до минуты, без предела это дешёвый DoS.
func (h *Hub) serveChannels(p *peer, channels <-chan ssh.NewChannel) {
	slots := make(chan struct{}, MaxMessagesInFlight)
	for c := range channels {
		if c.ChannelType() != messageChannel {
			c.Reject(ssh.Prohibited, "channel not permitted")
			continue
		}
		h.mu.Lock()
		router := h.router
		h.mu.Unlock()
		if router == nil || !h.currentPeer(p) {
			c.Reject(ssh.Prohibited, "channel not permitted")
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Reject(ssh.ResourceShortage, "swarm message limit reached")
			continue
		}
		go func(c ssh.NewChannel) {
			defer func() { <-slots }()
			serveMessage(router, p.id.uid, c)
		}(c)
	}
}

func (h *Hub) session(uid string, generation int64) *peer {
	id := identity{uid, generation}
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.peers[id]
	if p != nil && !h.authorize(uid, generation, p.token) {
		delete(h.peers, id)
		p.conn.Close()
		return nil
	}
	return p
}

// Connected reports only an authenticated, currently authorized incarnation.
func (h *Hub) Connected(uid string, generation int64) bool { return h.session(uid, generation) != nil }

// Dial opens one terminal stream. The context controls opening and the entire
// returned connection lifetime; callers must keep it alive until Close.
func (h *Hub) Dial(ctx context.Context, uid string, generation int64) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p := h.session(uid, generation)
	if p == nil {
		return nil, errUnavailable
	}
	h.mu.Lock()
	if h.closed || h.peers[p.id] != p || p.streams >= MaxStreams {
		h.mu.Unlock()
		return nil, errUnavailable
	}
	p.streams++
	h.mu.Unlock()
	release := func() { h.mu.Lock(); p.streams--; h.mu.Unlock() }
	type result struct {
		ch  ssh.Channel
		req <-chan *ssh.Request
		err error
	}
	out := make(chan result)
	go func() {
		ch, req, err := p.conn.OpenChannel(terminalChannel, nil)
		select {
		case out <- result{ch, req, err}:
		case <-ctx.Done():
			if ch != nil {
				ch.Close()
			}
			release()
		}
	}()
	select {
	case <-ctx.Done():
		// SSH has no channel-open cancellation message. Closing the peer is
		// necessary to unblock OpenChannel when the guest never replies.
		p.conn.Close()
		return nil, ctx.Err()
	case r := <-out:
		if r.err != nil {
			release()
			return nil, errUnavailable
		}
		go ssh.DiscardRequests(r.req)
		h.mu.Lock()
		valid := !h.closed && h.peers[p.id] == p && h.authorize(uid, generation, p.token)
		h.mu.Unlock()
		if !valid {
			r.ch.Close()
			release()
			return nil, errUnavailable
		}
		return newChannelConn(ctx, p.ctx, r.ch, p.conn.LocalAddr(), p.conn.RemoteAddr(), release), nil
	}
}

// Disconnect closes a particular incarnation and all its streams. The caller
// must revoke the durable token first if the guest must not reconnect.
func (h *Hub) Disconnect(uid string, generation int64) {
	h.mu.Lock()
	p := h.peers[identity{uid, generation}]
	delete(h.peers, identity{uid, generation})
	h.mu.Unlock()
	if p != nil {
		p.conn.Close()
	}
}

// Close stops accepting guests and closes active and handshaking connections.
func (h *Hub) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for id, p := range h.peers {
		p.conn.Close()
		delete(h.peers, id)
	}
	for ws := range h.pending {
		ws.Close()
	}
	return nil
}

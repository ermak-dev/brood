package relay

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/net/websocket"
)

// ClientConfig contains private machine credentials, never browser credentials.
// URL is a wss:// origin or the exact guest endpoint, without query, userinfo or
// fragment. HostKey is Hub.HostKey(). CAPEM supplies explicit trusted roots;
// if empty, system certificate roots are used. DialTarget is trusted local code
// that must dial only ttyd at 127.0.0.1:8080 and honor context cancellation. No
// remote channel request can supply an address or command.
type ClientConfig struct {
	URL        string
	ActorUID   string
	Generation int64
	Token      string `json:"-"`
	CAPEM      []byte `json:"-"`
	HostKey    string
	DialTarget func(context.Context) (net.Conn, error) `json:"-"`
	// MessageDeduplication advertises a durable handler on this connection only.
	// Set it only after the supervisor journal and handler are ready.
	MessageDeduplication bool   `json:"messageDeduplication,omitempty"`
	MessageJournalID     string `json:"messageJournalId,omitempty"`
	// HandleMessage receives server-authorized swarm messages. It must honor
	// cancellation and never dial actor-supplied socket paths as root.
	HandleMessage func(context.Context, MessageRequest) MessageResponse `json:"-"`
	// InboxReceiverID and HandleInbox opt in only this authenticated peer.
	InboxReceiverID string                                        `json:"inboxReceiverId,omitempty"`
	HandleInbox     func(context.Context, []byte) ([]byte, error) `json:"-"`
	OnSwarmSession  func(context.Context, SwarmCaller)            `json:"-"`
	// Session handles authenticated server session operations independently.
	Session func(context.Context, SessionRequest) SessionResponse `json:"-"`
	// SessionFactory overrides Session with fresh state for each authenticated
	// connection. Its context is canceled on disconnect, including reconnects.
	SessionFactory func(context.Context) func(context.Context, SessionRequest) SessionResponse `json:"-"`
}

func (ClientConfig) String() string     { return "relay.ClientConfig{redacted}" }
func (c ClientConfig) GoString() string { return c.String() }

// Format serves every verb: String and GoString cover only some, and the rest
// would print the fields. %p bypasses Format and prints an address only.
func (c ClientConfig) Format(s fmt.State, _ rune) { _, _ = io.WriteString(s, c.String()) }

type clientOptions struct {
	ws             *websocket.Config
	ssh            *ssh.ClientConfig
	target         func(context.Context) (net.Conn, error)
	message        func(context.Context, MessageRequest) MessageResponse
	inbox          func(context.Context, []byte) ([]byte, error)
	onSwarmSession func(context.Context, SwarmCaller)
	session        func(context.Context, SessionRequest) SessionResponse
	sessionFactory func(context.Context) func(context.Context, SessionRequest) SessionResponse
}

func options(c ClientConfig) (*clientOptions, error) {
	if c.MessageDeduplication && c.HandleMessage == nil {
		return nil, errors.New("message deduplication requires a message handler")
	}
	if c.MessageDeduplication && !validMessageJournalID(c.MessageJournalID) || !c.MessageDeduplication && c.MessageJournalID != "" {
		return nil, errors.New("message deduplication requires an explicit retained journal identity")
	}
	if c.HandleInbox == nil && c.InboxReceiverID != "" || c.HandleInbox != nil && !validMessageJournalID(c.InboxReceiverID) {
		return nil, errors.New("supervisor inbox requires an explicit retained receiver and handler")
	}
	if !validUID(c.ActorUID) || c.Generation <= 0 || !validToken(c.Token) || c.DialTarget == nil {
		return nil, errors.New("invalid guest relay configuration")
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" {
		return nil, errors.New("relay URL must be a WSS origin or guest endpoint without credentials or query")
	}
	path := route(identity{c.ActorUID, c.Generation})
	if u.Path != "" && u.Path != "/" && u.Path != path {
		return nil, errors.New("relay URL has an invalid guest endpoint")
	}
	u.Path = path
	key, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(c.HostKey))
	if err != nil || len(rest) != 0 {
		return nil, errors.New("invalid relay SSH host key")
	}
	var roots *x509.CertPool
	if len(c.CAPEM) > 0 {
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(c.CAPEM) {
			return nil, errors.New("invalid relay CA certificate")
		}
	} else {
		roots, err = x509.SystemCertPool()
		if err != nil {
			return nil, errors.New("relay system CA certificates unavailable")
		}
	}
	origin := *u
	origin.Scheme = "https"
	origin.Path = ""
	wc, err := websocket.NewConfig(u.String(), origin.String())
	if err != nil {
		return nil, errors.New("invalid relay websocket configuration")
	}
	wc.TlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: u.Hostname()}
	wc.Header.Set("Authorization", "Bearer "+c.Token)
	if c.MessageDeduplication {
		wc.Header.Set(messageProtocolHeader, messageDedupProtocol)
		wc.Header.Set(messageJournalHeader, c.MessageJournalID)
	}
	if c.HandleInbox != nil {
		wc.Header.Set(inboxProtocolHeader, inboxProtocol)
		wc.Header.Set(inboxReceiverHeader, c.InboxReceiverID)
	}
	sshConfig := &ssh.ClientConfig{User: c.ActorUID + "/" + strconv.FormatInt(c.Generation, 10), Auth: []ssh.AuthMethod{ssh.Password(c.Token)}, HostKeyCallback: ssh.FixedHostKey(key)}
	return &clientOptions{ws: wc, ssh: sshConfig, target: c.DialTarget, message: c.HandleMessage,
		inbox: c.HandleInbox, onSwarmSession: c.OnSwarmSession, session: c.Session, sessionFactory: c.SessionFactory}, nil
}

// RunClient reconnects with bounded exponential backoff until ctx is canceled.
// Every attempt has a finite TLS/WebSocket/SSH handshake deadline. Credentials
// are never put in URLs, output, JSON, or returned transport errors.
func RunClient(ctx context.Context, c ClientConfig) error {
	opts, err := options(c)
	if err != nil {
		return err
	}
	backoff := 100 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		connected := runAttempt(ctx, opts)
		if err := ctx.Err(); err != nil {
			return err
		}
		if connected {
			backoff = 100 * time.Millisecond
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		backoff = min(backoff*2, 5*time.Second)
	}
}
func runAttempt(ctx context.Context, o *clientOptions) bool {
	attempt, cancel := context.WithTimeout(ctx, handshakeTimeout)
	defer cancel()
	ws, err := o.ws.DialContext(attempt)
	if err != nil {
		return false
	}
	socket := newSocket(ws)
	defer socket.Close()
	stop := context.AfterFunc(ctx, func() { socket.Close() })
	defer stop()
	socket.SetDeadline(time.Now().Add(handshakeTimeout))
	conn, channels, requests, err := ssh.NewClientConn(socket, o.ws.Location.Host, o.ssh)
	if err != nil {
		return false
	}
	defer conn.Close()
	socket.SetDeadline(time.Time{})
	session, cancelSession := context.WithCancel(ctx)
	defer cancelSession()
	go ssh.DiscardRequests(requests)
	done := make(chan struct{})
	go func() { conn.Wait(); cancelSession(); close(done) }()
	if o.onSwarmSession != nil {
		go o.onSwarmSession(session, swarmCaller(conn, session))
	}
	handler := o.session
	if o.sessionFactory != nil {
		handler = o.sessionFactory(session)
	}
	serveTargets(session, channels, o.target, o.message, handler, o.inbox)
	<-done
	return true
}
func serveTargets(ctx context.Context, channels <-chan ssh.NewChannel, target func(context.Context) (net.Conn, error), message func(context.Context, MessageRequest) MessageResponse, session func(context.Context, SessionRequest) SessionResponse, inbox func(context.Context, []byte) ([]byte, error)) {
	var wg sync.WaitGroup
	defer wg.Wait()
	slots := make(chan struct{}, MaxStreams)
	messageSlots := make(chan struct{}, MaxMessagesInFlight)
	for ch := range channels {
		if ch.ChannelType() == inboxChannel && inbox != nil {
			select {
			case messageSlots <- struct{}{}:
			default:
				ch.Reject(ssh.ResourceShortage, "supervisor inbox stream limit reached")
				continue
			}
			wg.Go(func() {
				defer func() { <-messageSlots }()
				serveGuestInbox(ctx, inbox, ch)
			})
			continue
		}
		if ch.ChannelType() == sessionChannel && session != nil {
			select {
			case messageSlots <- struct{}{}:
			default:
				ch.Reject(ssh.ResourceShortage, "session stream limit reached")
				continue
			}
			wg.Add(1)
			go func(ch ssh.NewChannel) {
				defer wg.Done()
				defer func() { <-messageSlots }()
				serveGuestSession(ctx, session, ch)
			}(ch)
			continue
		}
		if ch.ChannelType() == messageChannel && message != nil {
			select {
			case messageSlots <- struct{}{}:
			default:
				ch.Reject(ssh.ResourceShortage, "message stream limit reached")
				continue
			}
			wg.Add(1)
			go func(ch ssh.NewChannel) {
				defer wg.Done()
				defer func() { <-messageSlots }()
				serveGuestMessage(ctx, message, ch)
			}(ch)
			continue
		}
		if ch.ChannelType() != terminalChannel || len(ch.ExtraData()) != 0 {
			ch.Reject(ssh.Prohibited, "channel not permitted")
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			ch.Reject(ssh.ResourceShortage, "terminal stream limit reached")
			continue
		}
		wg.Add(1)
		go func(ch ssh.NewChannel) {
			defer wg.Done()
			defer func() { <-slots }()
			dialCtx, cancel := context.WithTimeout(ctx, handshakeTimeout)
			dest, err := target(dialCtx)
			cancel()
			if err != nil {
				ch.Reject(ssh.ConnectionFailed, "terminal unavailable")
				return
			}
			defer dest.Close()
			stop := context.AfterFunc(ctx, func() { dest.Close() })
			defer stop()
			stream, req, err := ch.Accept()
			if err != nil {
				return
			}
			defer stream.Close()
			go ssh.DiscardRequests(req)
			finished := make(chan struct{})
			go func() {
				_, err := io.Copy(dest, stream)
				if err != nil {
					dest.Close()
					stream.Close()
				} else if cw, ok := dest.(interface{ CloseWrite() error }); ok {
					cw.CloseWrite()
				} else {
					dest.Close()
				}
				close(finished)
			}()
			_, err = io.Copy(stream, dest)
			if err != nil {
				dest.Close()
				stream.Close()
			} else {
				stream.CloseWrite()
			}
			<-finished
		}(ch)
	}
}

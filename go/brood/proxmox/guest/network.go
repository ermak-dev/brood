//go:build linux

package guest

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// RuleCommand executes nft with a script on stdin. It must not invoke a shell.
type RuleCommand func(context.Context, string, []string, string) error

func applyRules(ctx context.Context, script string, command RuleCommand) error {
	if command == nil {
		return errors.New("nft command is required")
	}
	if err := command(ctx, "/usr/sbin/nft", []string{"-f", "-"}, script); err != nil {
		return errors.Join(errors.New("guest nft transaction failed; requires guest CAP_NET_ADMIN and nft kernel support"), err)
	}
	return nil
}
func nftRules() string {
	// В хуке output oifname остается исходным маршрутом (eth0) и после
	// redirect на :5353, поэтому перенаправленный DNS актора не проходит
	// правила oifname "lo"; его пропускает только ct status dnat к локальному
	// резолверу, прямой трафик к 5353 по-прежнему только через lo.
	return `create table inet brood_guest
add chain inet brood_guest dns { type nat hook output priority dstnat; policy accept; }
add rule inet brood_guest dns meta skuid 1000 udp dport 53 redirect to :5353
add rule inet brood_guest dns meta skuid 1000 tcp dport 53 redirect to :5353
add chain inet brood_guest actor
add rule inet brood_guest actor oifname "lo" ip daddr 127.0.0.1 tcp dport { 3128, 5353 } accept
add rule inet brood_guest actor oifname "lo" ip daddr 127.0.0.1 udp dport 5353 accept
add rule inet brood_guest actor oifname "lo" ip6 daddr ::1 tcp dport 5353 accept
add rule inet brood_guest actor oifname "lo" ip6 daddr ::1 udp dport 5353 accept
add rule inet brood_guest actor ct status dnat ip daddr 127.0.0.1 udp dport 5353 accept
add rule inet brood_guest actor ct status dnat ip daddr 127.0.0.1 tcp dport 5353 accept
add rule inet brood_guest actor ct status dnat ip6 daddr ::1 udp dport 5353 accept
add rule inet brood_guest actor ct status dnat ip6 daddr ::1 tcp dport 5353 accept
add rule inet brood_guest actor oifname "lo" ip saddr 127.0.0.1 ip daddr 127.0.0.1 tcp sport 8080 ct direction reply ct state established accept
add rule inet brood_guest actor reject with icmpx type admin-prohibited
add chain inet brood_guest output { type filter hook output priority filter; policy accept; }
add rule inet brood_guest output meta skuid 1000 jump actor
add chain inet brood_guest input { type filter hook input priority filter; policy accept; }
add rule inet brood_guest input iifname != "lo" tcp dport { 3128, 5353, 8080 } reject with icmpx type admin-prohibited
add rule inet brood_guest input iifname != "lo" udp dport 5353 reject with icmpx type admin-prohibited
`
}
func systemRules(ctx context.Context, script string) error {
	return applyRules(ctx, script, func(ctx context.Context, name string, args []string, input string) error {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
		cmd.Stdin = strings.NewReader(input)
		return cmd.Run()
	})
}

// Services owns all root listeners; any unexpected serving failure stops the actor.
type Services interface {
	Done() <-chan error
	Close() error
}
type networkServices struct {
	cancel    context.CancelFunc
	done      chan error
	proxy     *Proxy
	http      *http.Server
	listeners []net.Listener
	packets   []net.PacketConn
	wg        sync.WaitGroup
	mu        sync.Mutex
	closed    bool
	conns     map[net.Conn]bool
	once      sync.Once
	slots     chan struct{}
}

func (s *networkServices) Done() <-chan error { return s.done }
func (s *networkServices) fail(err error) {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if !closed {
		select {
		case s.done <- err:
		default:
		}
	}
}
func (s *networkServices) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		for c := range s.conns {
			_ = c.Close()
		}
		s.mu.Unlock()
		s.cancel()
		for _, l := range s.listeners {
			_ = l.Close()
		}
		for _, p := range s.packets {
			_ = p.Close()
		}
		if s.http != nil {
			_ = s.http.Close()
		}
		if s.proxy != nil {
			_ = s.proxy.Close()
		}
		s.wg.Wait()
	})
	return nil
}
func (s *networkServices) track(c net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = c.Close()
		return false
	}
	s.conns[c] = true
	return true
}
func (s *networkServices) forget(c net.Conn) {
	_ = c.Close()
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}
func startServices(parent context.Context, p *Policy) (Services, error) {
	return startServicesWith(parent, p, net.Listen, net.ListenPacket)
}
func startServicesWith(parent context.Context, p *Policy, listen func(string, string) (net.Listener, error), listenPacket func(string, string) (net.PacketConn, error)) (Services, error) {
	ctx, cancel := context.WithCancel(parent)
	s := &networkServices{cancel: cancel, done: make(chan error, 1), conns: map[net.Conn]bool{}, slots: make(chan struct{}, 64)}
	success := false
	defer func() {
		if !success {
			_ = s.Close()
		}
	}()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	s.proxy, _ = NewProxy(p, dialer.DialContext)
	httpListener, err := listen("tcp4", "127.0.0.1:3128")
	if err != nil {
		return nil, errors.New("guest HTTP proxy bind failed")
	}
	s.listeners = append(s.listeners, httpListener)
	for _, address := range []string{"127.0.0.1:5353", "[::1]:5353"} {
		listener, err := listen("tcp", address)
		if err != nil {
			return nil, errors.New("guest DNS TCP bind failed")
		}
		s.listeners = append(s.listeners, listener)
		packet, err := listenPacket("udp", address)
		if err != nil {
			return nil, errors.New("guest DNS UDP bind failed")
		}
		s.packets = append(s.packets, packet)
	}
	s.http = &http.Server{Handler: s.proxy, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return ctx }}
	s.wg.Go(func() {
		if err := s.http.Serve(httpListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.fail(errors.New("guest HTTP proxy stopped"))
		}
	})
	for _, l := range s.listeners[1:] {
		s.wg.Go(func() { s.serveTCP(ctx, p, l) })
	}
	for _, packet := range s.packets {
		s.wg.Go(func() { s.serveUDP(ctx, p, packet) })
	}
	success = true
	return s, nil
}
func (s *networkServices) serveUDP(ctx context.Context, p *Policy, c net.PacketConn) {
	buffer := make([]byte, 4096)
	for {
		n, peer, err := c.ReadFrom(buffer)
		if err != nil {
			s.fail(errors.New("guest DNS UDP stopped"))
			return
		}
		reply, err := p.DNSReply(ctx, buffer[:n])
		if err == nil {
			if _, err = c.WriteTo(reply, peer); err != nil {
				s.fail(errors.New("guest DNS UDP reply failed"))
				return
			}
		}
	}
}
func (s *networkServices) serveTCP(ctx context.Context, p *Policy, l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			s.fail(errors.New("guest DNS TCP stopped"))
			return
		}
		if !s.track(conn) {
			return
		}
		select {
		case s.slots <- struct{}{}:
		default:
			s.forget(conn)
			continue
		}
		s.wg.Go(func() {
			defer func() { <-s.slots; s.forget(conn) }()
			for {
				_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
				var size [2]byte
				if _, err := io.ReadFull(conn, size[:]); err != nil {
					return
				}
				length := binary.BigEndian.Uint16(size[:])
				if length == 0 || length > 4096 {
					return
				}
				packet := make([]byte, length)
				if _, err := io.ReadFull(conn, packet); err != nil {
					return
				}
				reply, err := p.DNSReply(ctx, packet)
				if err != nil {
					return
				}
				binary.BigEndian.PutUint16(size[:], uint16(len(reply)))
				if _, err = conn.Write(append(size[:], reply...)); err != nil {
					return
				}
			}
		})
	}
}

//go:build linux

package guest

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Tests bind only owned ephemeral loopback sockets, never fixed guest ports.
func ephemeralTCP(_, _ string) (net.Listener, error) { return net.Listen("tcp4", "127.0.0.1:0") }
func ephemeralUDP(_, _ string) (net.PacketConn, error) {
	return net.ListenPacket("udp4", "127.0.0.1:0")
}
func TestDNSListeners(t *testing.T) {
	p, _ := NewPolicy([]string{"example.com"}, publicLookup)
	raw, err := startServicesWith(context.Background(), p, ephemeralTCP, ephemeralUDP)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	s := raw.(*networkServices)
	query := dnsQuery(t, "example.com.", dnsmessage.TypeA)
	tcp, err := net.Dial("tcp", s.listeners[1].Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	_ = tcp.SetDeadline(time.Now().Add(2 * time.Second))
	prefix := make([]byte, 2)
	binary.BigEndian.PutUint16(prefix, uint16(len(query)))
	_, _ = tcp.Write(append(prefix, query...))
	if _, err = io.ReadFull(tcp, prefix); err != nil {
		t.Fatal(err)
	}
	reply := make([]byte, binary.BigEndian.Uint16(prefix))
	if _, err = io.ReadFull(tcp, reply); err != nil {
		t.Fatal(err)
	}
	var answer dnsmessage.Message
	if err = answer.Unpack(reply); err != nil || len(answer.Answers) != 1 {
		t.Fatalf("%v %v", answer, err)
	}
	udp, err := net.Dial("udp", s.packets[0].LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	_ = udp.SetDeadline(time.Now().Add(2 * time.Second))
	_, _ = udp.Write(query)
	reply = make([]byte, 4096)
	n, err := udp.Read(reply)
	if err != nil {
		t.Fatal(err)
	}
	if err = answer.Unpack(reply[:n]); err != nil || len(answer.Answers) != 1 {
		t.Fatalf("%v %v", answer, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-s.Done():
		t.Fatalf("normal cleanup reported failure: %v", err)
	default:
	}
}
func TestListenerBindFailureClosesEverything(t *testing.T) {
	p, _ := NewPolicy([]string{"example.com"}, publicLookup)
	var listeners []net.Listener
	calls := 0
	raw, err := startServicesWith(context.Background(), p, func(_, _ string) (net.Listener, error) {
		calls++
		if calls == 3 {
			return nil, errors.New("IPv6 missing")
		}
		l, err := ephemeralTCP("", "")
		if err == nil {
			listeners = append(listeners, l)
		}
		return l, err
	}, ephemeralUDP)
	if err == nil || raw != nil {
		t.Fatal("partial services accepted")
	}
	for _, l := range listeners {
		if c, err := net.DialTimeout("tcp", l.Addr().String(), time.Second); err == nil {
			c.Close()
			t.Fatal("failed bind leaked listener")
		}
	}
}
func TestServiceFailureNotifiesSupervisor(t *testing.T) {
	p, _ := NewPolicy([]string{"example.com"}, publicLookup)
	raw, err := startServicesWith(context.Background(), p, ephemeralTCP, ephemeralUDP)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	s := raw.(*networkServices)
	_ = s.packets[0].Close()
	select {
	case err := <-s.Done():
		if err == nil {
			t.Fatal("missing failure reason")
		}
	case <-time.After(time.Second):
		t.Fatal("DNS failure hidden")
	}
}

// chainRules возвращает выражения правил цепочки в порядке скрипта.
func chainRules(script, chain string) []string {
	prefix := "add rule inet brood_guest " + chain + " "
	var rules []string
	for line := range strings.Lines(script) {
		if rule, ok := strings.CutPrefix(strings.TrimSpace(line), prefix); ok {
			rules = append(rules, rule)
		}
	}
	return rules
}

// BUG: в непривилегированном CT актор не резолвил даже одобренные имена:
// nslookup получал EPERM. В хуке output oifname остается исходным (eth0)
// и после redirect на :5353, поэтому правила oifname "lo" не пропускают
// перенаправленный DNS, и он упирается в финальный reject.
// SPEC: до reject цепочка actor принимает перенаправленный (ct status dnat)
// DNS только к локальному резолверу 127.0.0.1:5353 и [::1]:5353, UDP и TCP;
// остальные правила цепочки не меняются.
func TestActorAcceptsOnlyRedirectedDNS(t *testing.T) {
	redirected := []string{
		"ct status dnat ip daddr 127.0.0.1 udp dport 5353 accept",
		"ct status dnat ip daddr 127.0.0.1 tcp dport 5353 accept",
		"ct status dnat ip6 daddr ::1 udp dport 5353 accept",
		"ct status dnat ip6 daddr ::1 tcp dport 5353 accept",
	}
	direct := []string{
		`oifname "lo" ip daddr 127.0.0.0/8 meta l4proto { tcp, udp } accept`,
		`oifname "lo" ip6 daddr ::1 meta l4proto { tcp, udp } accept`,
		"reject with icmpx type admin-prohibited",
	}
	script := nftRules()
	actor := chainRules(script, "actor")
	reject := slices.Index(actor, "reject with icmpx type admin-prohibited")
	if reject != len(actor)-1 {
		t.Fatalf("actor chain must end with its reject: %q", actor)
	}
	for _, rule := range redirected {
		at := slices.Index(actor, rule)
		if at < 0 || at > reject {
			t.Errorf("missing before reject: %s", rule)
		}
		if at >= 0 && slices.Index(actor[at+1:], rule) >= 0 {
			t.Errorf("duplicated: %s", rule)
		}
	}
	rest := slices.DeleteFunc(slices.Clone(actor), func(rule string) bool { return slices.Contains(redirected, rule) })
	if !slices.Equal(rest, direct) {
		t.Errorf("direct actor rules changed:\n%q\nwant\n%q", rest, direct)
	}
	for line := range strings.Lines(script) {
		line = strings.TrimSpace(line)
		rule, inActor := strings.CutPrefix(line, "add rule inet brood_guest actor ")
		if strings.Contains(line, "ct status") && (!inActor || !slices.Contains(redirected, rule)) {
			t.Errorf("redirected traffic accepted beyond the local resolver: %s", line)
		}
	}
}

// BUG: внутри CT актора httptest-серверы пакетов server/clientapi/bro не
// запускались: цепочка actor принимала по lo только фиксированные порты root
// сервисов, и SYN UID 1000 к эфемерному порту 127.0.0.1 (как и SYN-ACK его
// собственного listener'а) упирался в финальный reject (#250).
// SPEC: до reject цепочка actor принимает весь TCP и UDP UID 1000 через lo
// к 127.0.0.0/8 и ::1 (UDP нужен тестам резолвера guest); каждый accept
// цепочки либо ограничен oifname "lo", либо является dnat к локальному
// резолверу, поэтому egress вне lo по-прежнему заканчивается reject; цепочки
// input и dns не меняются.
func TestActorAcceptsLoopbackTCPAndUDP(t *testing.T) {
	loopback := []string{
		`oifname "lo" ip daddr 127.0.0.0/8 meta l4proto { tcp, udp } accept`,
		`oifname "lo" ip6 daddr ::1 meta l4proto { tcp, udp } accept`,
	}
	script := nftRules()
	actor := chainRules(script, "actor")
	reject := slices.Index(actor, "reject with icmpx type admin-prohibited")
	if reject != len(actor)-1 {
		t.Fatalf("actor chain must end with its reject: %q", actor)
	}
	for _, rule := range loopback {
		if at := slices.Index(actor, rule); at < 0 || at > reject {
			t.Errorf("missing before reject: %s", rule)
		}
	}
	for _, rule := range actor[:reject] {
		if !strings.HasSuffix(rule, " accept") {
			t.Errorf("unexpected actor verdict: %s", rule)
		}
		if !strings.HasPrefix(rule, `oifname "lo" `) && !strings.HasPrefix(rule, "ct status dnat ") {
			t.Errorf("egress accepted beyond loopback: %s", rule)
		}
		if strings.HasPrefix(rule, "ct status dnat ") && !strings.Contains(rule, " dport 5353 ") {
			t.Errorf("redirected traffic accepted beyond the local resolver: %s", rule)
		}
	}
	input := []string{
		`iifname != "lo" tcp dport { 3128, 5353, 8080, 8317, 8318 } reject with icmpx type admin-prohibited`,
		`iifname != "lo" udp dport 5353 reject with icmpx type admin-prohibited`,
	}
	if got := chainRules(script, "input"); !slices.Equal(got, input) {
		t.Errorf("input chain changed:\n%q\nwant\n%q", got, input)
	}
	dns := []string{
		"meta skuid 1000 udp dport 53 redirect to :5353",
		"meta skuid 1000 tcp dport 53 redirect to :5353",
	}
	if got := chainRules(script, "dns"); !slices.Equal(got, dns) {
		t.Errorf("dns chain changed:\n%q\nwant\n%q", got, dns)
	}
	if got := chainRules(script, "output"); !slices.Equal(got, []string{"meta skuid 1000 jump actor"}) {
		t.Errorf("output chain changed: %q", got)
	}
}
func TestMalformedProxyAuthority(t *testing.T) {
	p, _ := NewPolicy([]string{"example.com"}, publicLookup)
	proxy, _ := NewProxy(p, func(context.Context, string, string) (net.Conn, error) {
		t.Error("invalid authority dialed")
		return nil, errors.New("denied")
	})
	for _, authority := range []string{"example.com:", "example.com:abc", "example.com::80"} {
		req := httptest.NewRequest("GET", "http://example.com/", nil)
		req.URL.Host = authority
		req.Host = authority
		rec := httptest.NewRecorder()
		proxy.ServeHTTP(rec, req)
		if rec.Code != 403 {
			t.Errorf("%q: %d", authority, rec.Code)
		}
	}
}
func TestPartialLaunchAndRuleRemovalFailure(t *testing.T) {
	var events []string
	c, _ := testConfig(&events)
	child := &fakeProcess{done: make(chan struct{}), events: &events}
	c.Dependencies.Launch = func(context.Context, ActorSpec) (Process, error) { return child, errors.New("helper failed") }
	if h, err := Start(context.Background(), c); h != nil || err == nil {
		t.Fatal("partial launch accepted")
	}
	if !reflect.DeepEqual(events, []string{"workspace", "rules-install", "services-start", "process-close", "services-close", "rules-delete"}) {
		t.Fatal(events)
	}
	events = nil
	c, _ = testConfig(&events)
	original := c.Dependencies.Rules
	c.Dependencies.Rules = func(ctx context.Context, s string) error {
		if strings.HasPrefix(s, "delete") {
			return errors.New("nft cleanup refused")
		}
		return original(ctx, s)
	}
	h, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err == nil || !strings.Contains(err.Error(), "nft cleanup refused") {
		t.Fatal(err)
	}
}

//go:build linux

// Package guest supervises the nonroot Proxmox coding profile inside a dedicated CT.
package guest

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"time"

	"brood.dev/go/brood/proxmox/egress"
	"golang.org/x/net/dns/dnsmessage"
)

// Lookup runs as the trusted supervisor using the DHCP-provided resolver.
type Lookup func(context.Context, string) ([]net.IP, error)

// Policy permits only explicitly configured DNS names and public addresses.
type Policy struct {
	hosts   map[string]bool
	lookup  Lookup
	network *egress.Policy
}

// NewPolicy rejects wildcards, IP literals and malformed names.
func NewPolicy(hosts []string, lookup Lookup) (*Policy, error) {
	if len(hosts) == 0 || lookup == nil {
		return nil, errors.New("guest requires approved hosts and a resolver")
	}
	p := &Policy{hosts: map[string]bool{}, lookup: lookup}
	for _, host := range hosts {
		h, err := canonicalHost(host)
		if err != nil {
			return nil, err
		}
		p.hosts[h] = true
	}
	return p, nil
}

// NewNetworkPolicy preserves selected protocol and port rules, including offline.
// Address validation remains public-only; DNS approval alone never permits a dial.
func NewNetworkPolicy(rules []egress.Rule, lookup Lookup) (*Policy, error) {
	if lookup == nil {
		return nil, errors.New("guest requires a resolver")
	}
	network, err := egress.Compile(rules)
	if err != nil {
		return nil, err
	}
	p := &Policy{hosts: map[string]bool{}, lookup: lookup, network: network}
	for _, rule := range network.Rules() {
		for _, host := range rule.Hostnames {
			p.hosts[host] = true
		}
	}
	return p, nil
}

func canonicalHost(host string) (string, error) {
	return egress.Hostname(host)
}

// Resolve validates every returned address before allowing any dial or DNS answer.
func (p *Policy) Resolve(ctx context.Context, host string) ([]net.IP, error) {
	h, err := canonicalHost(host)
	if err != nil || !p.hosts[h] {
		return nil, errors.New("host is not approved")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ips, err := p.lookup(ctx, h)
	if err != nil || len(ips) == 0 {
		return nil, errors.New("approved host resolution failed")
	}
	result := make([]net.IP, 0, len(ips))
	for _, ip := range ips {
		if !publicIP(ip) {
			return nil, errors.New("approved host resolved to a nonpublic address")
		}
		result = append(result, append(net.IP(nil), ip...))
	}
	return result, nil
}
func publicIP(ip net.IP) bool {
	a, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	a = a.Unmap()
	if !a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() {
		return false
	}
	if a.Is6() && !netip.MustParsePrefix("2000::/3").Contains(a) {
		return false
	}
	for _, block := range []string{"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "240.0.0.0/4", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"} {
		if netip.MustParsePrefix(block).Contains(a) {
			return false
		}
	}
	return true
}

// DNSReply refuses all but single-question A/AAAA queries for approved public names.
func (p *Policy) DNSReply(ctx context.Context, packet []byte) ([]byte, error) {
	var q dnsmessage.Message
	if err := q.Unpack(packet); err != nil {
		return nil, errors.New("malformed DNS query")
	}
	if q.Response || q.OpCode != 0 || len(q.Questions) != 1 || len(q.Answers) != 0 || len(q.Authorities) != 0 {
		return nil, errors.New("unsupported DNS query")
	}
	r := dnsmessage.Message{Header: dnsmessage.Header{ID: q.ID, Response: true, RecursionDesired: q.RecursionDesired, RecursionAvailable: true, RCode: dnsmessage.RCodeRefused}, Questions: q.Questions}
	question := q.Questions[0]
	if question.Class != dnsmessage.ClassINET || (question.Type != dnsmessage.TypeA && question.Type != dnsmessage.TypeAAAA) {
		return r.Pack()
	}
	ips, err := p.Resolve(ctx, question.Name.String())
	if err != nil {
		return r.Pack()
	}
	r.RCode = dnsmessage.RCodeSuccess
	for _, ip := range ips {
		header := dnsmessage.ResourceHeader{Name: question.Name, Class: dnsmessage.ClassINET, Type: question.Type, TTL: 30}
		if v4 := ip.To4(); v4 != nil && question.Type == dnsmessage.TypeA {
			var a [4]byte
			copy(a[:], v4)
			r.Answers = append(r.Answers, dnsmessage.Resource{Header: header, Body: &dnsmessage.AResource{A: a}})
		} else if v4 == nil && question.Type == dnsmessage.TypeAAAA {
			var a [16]byte
			copy(a[:], ip.To16())
			r.Answers = append(r.Answers, dnsmessage.Resource{Header: header, Body: &dnsmessage.AAAAResource{AAAA: a}})
		}
	}
	return r.Pack()
}

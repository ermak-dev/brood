// Package egress validates immutable managed-guest destination policies.
package egress

import (
	"errors"
	"net"
	"slices"
	"strings"
)

// HTTP and TLS distinguish plaintext HTTP from TLS passthrough destinations.
const (
	HTTP = "http"
	TLS  = "tls"
)

// Rule allows exactly the Cartesian product of its hostnames and ports.
type Rule struct {
	Protocol  string   `json:"protocol"`
	Hostnames []string `json:"hostnames"`
	Ports     []int    `json:"ports"`
}

type endpoint struct {
	protocol, host string
	port           int
}

// Policy owns its canonical rules and permits no destination by default.
type Policy struct {
	rules     []Rule
	endpoints map[endpoint]bool
	hosts     map[string]bool
}

// Compile validates a bounded policy without resolving or contacting any host.
// An empty policy explicitly denies application egress.
func Compile(input []Rule) (*Policy, error) {
	if len(input) > 64 {
		return nil, errors.New("egress policy has too many rules")
	}
	p := &Policy{rules: make([]Rule, 0, len(input)), endpoints: map[endpoint]bool{}, hosts: map[string]bool{}}
	for _, rule := range input {
		if rule.Protocol != HTTP && rule.Protocol != TLS {
			return nil, errors.New("unsupported egress protocol")
		}
		if len(rule.Hostnames) == 0 || len(rule.Hostnames) > 256 || len(rule.Ports) == 0 || len(rule.Ports) > 256 {
			return nil, errors.New("egress rule requires bounded hostnames and ports")
		}
		normalized := Rule{Protocol: rule.Protocol, Ports: slices.Clone(rule.Ports)}
		for _, host := range rule.Hostnames {
			h, err := Hostname(host)
			if err != nil {
				return nil, err
			}
			normalized.Hostnames = append(normalized.Hostnames, h)
		}
		slices.Sort(normalized.Hostnames)
		normalized.Hostnames = slices.Compact(normalized.Hostnames)
		slices.Sort(normalized.Ports)
		normalized.Ports = slices.Compact(normalized.Ports)
		for _, port := range normalized.Ports {
			if port < 1 || port > 65535 {
				return nil, errors.New("invalid egress port")
			}
			for _, host := range normalized.Hostnames {
				p.endpoints[endpoint{rule.Protocol, host, port}] = true
				if len(p.endpoints) > 4096 {
					return nil, errors.New("egress policy has too many destinations")
				}
				p.hosts[host] = true
			}
		}
		p.rules = append(p.rules, normalized)
	}
	return p, nil
}

// Hostname canonicalizes an exact DNS name, rejecting URLs, literals and patterns.
func Hostname(host string) (string, error) {
	for _, c := range host {
		if c > 127 {
			return "", errors.New("invalid egress hostname")
		}
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if len(h) > 253 || !strings.Contains(h, ".") || net.ParseIP(h) != nil {
		return "", errors.New("invalid egress hostname")
	}
	labels := strings.Split(h, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", errors.New("invalid egress hostname")
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return "", errors.New("invalid egress hostname")
			}
		}
	}
	for _, c := range labels[len(labels)-1] {
		if c >= 'a' && c <= 'z' {
			return h, nil
		}
	}
	return "", errors.New("invalid egress hostname")
}

// Allows checks the protocol, canonical DNS name and port as one destination.
func (p *Policy) Allows(protocol, host string, port int) bool {
	h, err := Hostname(host)
	return p != nil && err == nil && p.endpoints[endpoint{protocol, h, port}]
}

// AllowsDNS reports whether any destination rule names the host.
// It does not authorize a dial or relax address validation.
func (p *Policy) AllowsDNS(host string) bool {
	h, err := Hostname(host)
	return p != nil && err == nil && p.hosts[h]
}

// Rules returns a detached canonical snapshot, including an explicit empty slice.
func (p *Policy) Rules() []Rule {
	rules := make([]Rule, len(p.rules))
	for i, r := range p.rules {
		rules[i] = Rule{Protocol: r.Protocol, Hostnames: slices.Clone(r.Hostnames), Ports: slices.Clone(r.Ports)}
	}
	return rules
}

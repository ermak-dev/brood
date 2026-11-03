// Package gitorigin canonicalizes and classifies the HTTPS Git origin of a
// managed actor. It is pure: no network, DNS, configuration or credentials.
// Errors describe the violated rule and never echo the submitted value, which
// may contain userinfo.
package gitorigin

import (
	"errors"
	"strings"
)

// MaxLength bounds the submitted and canonical origin in bytes.
const MaxLength = 1024

// Access modes of a canonical origin.
const (
	// ModeGateway: the server mediates Git with an installation credential.
	ModeGateway = "gateway"
	// ModeDirect: the guest clones through its public egress proxy.
	ModeDirect = "direct"
)

var (
	errLength    = errors.New("origin must be at most 1024 bytes")
	errText      = errors.New("origin must contain only visible ASCII characters")
	errScheme    = errors.New("origin must be an https:// URL")
	errUserinfo  = errors.New("origin must not contain credentials")
	errQuery     = errors.New("origin must not contain a query, fragment or percent-encoding")
	errPort      = errors.New("origin must not specify a port other than 443")
	errHost      = errors.New("origin host must be a DNS name, not an IP address")
	errPath      = errors.New("origin requires a repository path")
	errSegment   = errors.New("origin path segments must be nonempty names of letters, digits, '.', '_', '~' or '-' and not '.' or '..'")
	errMalformed = errors.New("origin is malformed")
)

// Canonical validates raw and returns its canonical form: lowercase scheme
// and host, no default port and no trailing slash. The path keeps its case.
func Canonical(raw string) (string, error) {
	if len(raw) > MaxLength {
		return "", errLength
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] < 0x21 || raw[i] > 0x7e {
			return "", errText
		}
	}
	if len(raw) < len("https://") || !strings.EqualFold(raw[:len("https://")], "https://") {
		return "", errScheme
	}
	rest := raw[len("https://"):]
	if strings.ContainsAny(rest, "?#%") {
		return "", errQuery
	}
	authority, path, found := strings.Cut(rest, "/")
	if !found {
		return "", errPath
	}
	if strings.Contains(authority, "@") {
		return "", errUserinfo
	}
	host, port, hasPort := strings.Cut(authority, ":")
	if hasPort && port != "443" {
		return "", errPort
	}
	host = strings.ToLower(host)
	if !dnsName(host) {
		return "", errHost
	}
	path = strings.TrimSuffix(path, "/")
	if path == "" {
		return "", errPath
	}
	for _, segment := range strings.Split(path, "/") {
		if !pathSegment(segment) {
			return "", errSegment
		}
	}
	canonical := "https://" + host + "/" + path
	if len(canonical) > MaxLength {
		return "", errMalformed
	}
	return canonical, nil
}

// Host returns the hostname of a canonical origin, or "" for any other input.
func Host(origin string) string {
	canonical, err := Canonical(origin)
	if err != nil || canonical != origin {
		return ""
	}
	host, _, _ := strings.Cut(strings.TrimPrefix(origin, "https://"), "/")
	return host
}

// Mode classifies a canonical origin against current policy. Gateway requires
// exact equality with the project repository and an installation remote for
// it; direct requires a public host. Any other origin has no mode ("").
func Mode(origin, repository string, configured, public func(string) bool) string {
	host := Host(origin)
	if host == "" {
		return ""
	}
	if origin == repository && configured(origin) {
		return ModeGateway
	}
	if public(host) {
		return ModeDirect
	}
	return ""
}

// dnsName accepts lowercase LDH labels of 1..63 bytes, at most 253 bytes in
// total. The last label must start with a letter, which excludes IPv4 and
// numeric shorthand forms such as 127.1 or 0x7f000001.
func dnsName(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	labels := strings.Split(host, ".")
	for _, label := range labels {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	last := labels[len(labels)-1]
	return last[0] >= 'a' && last[0] <= 'z'
}

func pathSegment(segment string) bool {
	if segment == "" || segment == "." || segment == ".." {
		return false
	}
	for i := 0; i < len(segment); i++ {
		c := segment[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '~' || c == '-') {
			return false
		}
	}
	return true
}

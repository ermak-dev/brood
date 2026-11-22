// Package gitorigin canonicalizes the direct HTTPS or SSH Git origin of a
// managed actor. It is pure: no network, DNS, configuration or credentials.
// Errors describe the violated rule and never echo the submitted value, which
// may contain userinfo.
package gitorigin

import (
	"errors"
	"strconv"
	"strings"
)

// MaxLength bounds the submitted and canonical origin in bytes.
const MaxLength = 1024

var (
	errLength    = errors.New("origin must be at most 1024 bytes")
	errText      = errors.New("origin must contain only visible ASCII characters")
	errScheme    = errors.New("origin must be an https:// or ssh://git@ repository URL")
	errUserinfo  = errors.New("origin must not contain credentials")
	errQuery     = errors.New("origin must not contain a query, fragment or percent-encoding")
	errPort      = errors.New("origin must specify a valid port (HTTPS uses 443)")
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
	scheme, rest, ok := strings.Cut(raw, "://")
	scheme = strings.ToLower(scheme)
	if !ok || (scheme != "ssh" && scheme != "https") {
		return "", errScheme
	}
	if strings.ContainsAny(rest, "?#%") {
		return "", errQuery
	}
	authority, path, found := strings.Cut(rest, "/")
	if !found {
		return "", errPath
	}
	if scheme == "ssh" {
		var ok bool
		authority, ok = strings.CutPrefix(authority, "git@")
		if !ok {
			return "", errors.New("SSH origin must use the git user")
		}
	}
	if strings.Contains(authority, "@") {
		return "", errUserinfo
	}
	host, port, hasPort := strings.Cut(authority, ":")
	suffix := ""
	if hasPort {
		number, err := strconv.Atoi(port)
		if err != nil || strconv.Itoa(number) != port || number < 1 || number > 65535 || (scheme == "https" && number != 443) {
			return "", errPort
		}
		if scheme == "ssh" && number != 22 {
			suffix = ":" + port
		}
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
	if scheme == "ssh" {
		host = "git@" + host
	}
	canonical := scheme + "://" + host + suffix + "/" + path
	if len(canonical) > MaxLength {
		return "", errMalformed
	}
	return canonical, nil
}

// FromRemote accepts a URL or the conventional git@host:path syntax, preserving SSH.
func FromRemote(raw string) (string, error) {
	if len(raw) > MaxLength {
		return "", errLength
	}
	if hostPath, ok := strings.CutPrefix(raw, "git@"); ok {
		host, path, found := strings.Cut(hostPath, ":")
		if !found || strings.Contains(host, "/") {
			return "", errMalformed
		}
		return Canonical("ssh://git@" + host + "/" + strings.TrimPrefix(path, "/"))
	}
	return Canonical(raw)
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

package gitorigin

import (
	"strings"
	"testing"
)

// FEATURE: Canonical HTTPS Git origins for managed actors (#146).
// SPEC: Only https with a DNS hostname, optional port 443 and a nonempty clean
// path; no userinfo, query, fragment, controls or percent-encoding; at most
// 1024 bytes; lowercase host without a trailing slash. Errors never echo input.
// IMPLEMENTATION: Manual byte-level parsing instead of net/url normalization.
// RESULT: IMPLEMENTED — accepted forms canonicalize idempotently.
// STATUS: DONE
func TestCanonicalAcceptsAndNormalizes(t *testing.T) {
	for input, want := range map[string]string{
		"https://git.example.com/group/repo.git":      "https://git.example.com/group/repo.git",
		"https://Git.Example.COM/Group/Repo.git":      "https://git.example.com/Group/Repo.git",
		"HTTPS://git.example.com/group/repo":          "https://git.example.com/group/repo",
		"https://git.example.com:443/group/repo.git":  "https://git.example.com/group/repo.git",
		"https://git.example.com/group/repo.git/":     "https://git.example.com/group/repo.git",
		"https://github.com/owner/repo":               "https://github.com/owner/repo",
		"https://gitlab.com/a/b/c/d.git":              "https://gitlab.com/a/b/c/d.git",
		"https://xn--bcher-kva.example/~user/my_repo": "https://xn--bcher-kva.example/~user/my_repo",
		"https://git-1.example.com/a-b/c.d_e~f":       "https://git-1.example.com/a-b/c.d_e~f",
		"https://localhost/repo":                      "https://localhost/repo",
	} {
		got, err := Canonical(input)
		if err != nil || got != want {
			t.Fatalf("Canonical(%q) = %q, %v; want %q", input, got, err, want)
		}
		again, err := Canonical(got)
		if err != nil || again != got {
			t.Fatalf("canonical form is not idempotent: %q -> %q, %v", got, again, err)
		}
	}
	path := "https://git.example.com/" + strings.Repeat("a", 1024-len("https://git.example.com/"))
	if got, err := Canonical(path); err != nil || got != path {
		t.Fatalf("1024-byte origin rejected: %v", err)
	}
}

func TestCanonicalRejectsUnsafeOrigins(t *testing.T) {
	long := "https://git.example.com/" + strings.Repeat("a", 1025-len("https://git.example.com/"))
	for _, input := range []string{
		"",
		"http://git.example.com/repo",
		"ssh://git@git.example.com/repo",
		"git@git.example.com:group/repo.git",
		"file:///srv/repo.git",
		"https:git.example.com/repo",
		"//git.example.com/repo",
		"https:///repo",
		"https://git.example.com",
		"https://git.example.com/",
		"https://git.example.com//",
		"https://user@git.example.com/repo",
		"https://user:secret-sentinel@git.example.com/repo",
		"https://:secret-sentinel@git.example.com/repo",
		"https://git.example.com/repo?private=1",
		"https://git.example.com/repo?",
		"https://git.example.com/repo#main",
		"https://git.example.com/repo#",
		"https://git.example.com:8443/repo",
		"https://git.example.com:/repo",
		"https://git.example.com:0443/repo",
		"https://git.example.com:443:443/repo",
		"https://192.168.1.66/repo",
		"https://127.1/repo",
		"https://0x7f000001/repo",
		"https://[::1]/repo",
		"https://[fe80::1%25eth0]/repo",
		"https://git.example.com./repo",
		"https://.example.com/repo",
		"https://-git.example.com/repo",
		"https://git-.example.com/repo",
		"https://git_lab.example.com/repo",
		"https://" + strings.Repeat("a", 64) + ".example.com/repo",
		"https://" + strings.Repeat("a.", 127) + "com/repo",
		"https://gït.example.com/repo",
		"https://git.example.com/rëpo",
		"https://git.example.com/a//b",
		"https://git.example.com/a/./b",
		"https://git.example.com/a/../b",
		"https://git.example.com/..",
		"https://git.example.com/.",
		"https://git.example.com/a%2Fb",
		"https://git.example.com/a%20b",
		"https://git.example.com/a b",
		"https://git.example.com/a\\b",
		"https://git.example.com/a\x00b",
		"https://git.example.com/a\nb",
		"https://git.example.com/a\rb",
		"https://git.example.com/a\tb",
		"https://git.example.com/a\x7fb",
		" https://git.example.com/repo",
		"https://git.example.com/repo ",
		"https://git.example.com/a:b",
		"https://git.example.com/a@b",
		"https://git.example.com/a;b",
		"https://git.example.com/repo//",
		long,
	} {
		got, err := Canonical(input)
		if err == nil {
			t.Fatalf("Canonical(%q) accepted as %q", input, got)
		}
		if got != "" {
			t.Fatalf("rejected origin returned a value: %q", got)
		}
		if strings.Contains(err.Error(), "secret-sentinel") || (input != "" && strings.Contains(err.Error(), input)) {
			t.Fatalf("error echoes input: %v", err)
		}
	}
}

// SPEC: gateway requires exact equality with the project repository and an
// installation remote; direct requires a public allowlisted host; otherwise
// the origin has no access mode. An empty or noncanonical origin has no mode.
func TestMode(t *testing.T) {
	repository := "https://git.example.com/group/repo.git"
	configured := func(origin string) bool { return origin == repository || origin == "https://github.com/org/private" }
	public := func(host string) bool { return host == "github.com" || host == "gitlab.com" }
	for _, tc := range []struct{ origin, repository, want string }{
		{repository, repository, ModeGateway},
		{"https://github.com/org/private", "https://github.com/org/private", ModeGateway},
		{"https://github.com/org/private", repository, ModeDirect},
		{"https://github.com/owner/public", repository, ModeDirect},
		{"https://gitlab.com/owner/public", "", ModeDirect},
		{repository, "", ""},
		{repository, "https://git.example.com/group/other.git", ""},
		{"https://git.example.com/group/other.git", repository, ""},
		{"https://unlisted.example/repo", repository, ""},
		{"https://api.github.com.evil.example/repo", repository, ""},
		{"", repository, ""},
		{"", "", ""},
		{"https://GitHub.com/owner/public", repository, ""},
		{"https://github.com:443/owner/public", repository, ""},
		{"https://github.com/owner/public/", repository, ""},
	} {
		if got := Mode(tc.origin, tc.repository, configured, public); got != tc.want {
			t.Fatalf("Mode(%q, %q) = %q; want %q", tc.origin, tc.repository, got, tc.want)
		}
	}
	unconfigured := func(string) bool { return false }
	if got := Mode(repository, repository, unconfigured, public); got != "" {
		t.Fatalf("unconfigured repository received %q", got)
	}
}

func TestHost(t *testing.T) {
	if got := Host("https://git.example.com/group/repo.git"); got != "git.example.com" {
		t.Fatalf("Host = %q", got)
	}
	for _, input := range []string{"", "https://Git.example.com/repo", "http://git.example.com/repo", "https://git.example.com:443/repo"} {
		if got := Host(input); got != "" {
			t.Fatalf("Host(%q) = %q; want empty for a noncanonical origin", input, got)
		}
	}
}

package gitorigin

import (
	"strings"
	"testing"
)

// FEATURE: Normalize checkout remotes for runtime clones.
// SPEC: Common SSH forms become HTTPS; reject unsafe inputs without echoing them.
// IMPLEMENTATION: FromRemote delegates HTTPS validation to the existing canonicalizer.
// RESULT: IMPLEMENTED — default SSH ports normalize; unsafe remotes are redacted.
// STATUS: DONE
func TestFromRemote(t *testing.T) {
	for _, raw := range []string{"HTTPS://Git.Example.com:443/Team/repo.git/", "git@Git.Example.com:Team/repo.git", "ssh://git@Git.Example.com/Team/repo.git", "ssh://git@Git.Example.com:22/Team/repo.git"} {
		got, err := FromRemote(raw)
		if err != nil || got != "https://git.example.com/Team/repo.git" {
			t.Fatalf("canonical remote = %q, %v", got, err)
		}
	}
	for _, raw := range []string{"", "/tmp/repo", "file:///tmp/repo", "http://git.example.com/repo", "ssh://git@git.example.com:2222/repo", "ssh://git:DO_NOT_ECHO@git.example.com/repo", "https://DO_NOT_ECHO@git.example.com/repo", "https://git.example.com/repo?DO_NOT_ECHO", "git@git.example.com:repo#DO_NOT_ECHO", "git@127.0.0.1:repo", "ssh://git@[::1]/repo", "ssh://git@git.example.com/%2e%2e/repo"} {
		got, err := FromRemote(raw)
		if err == nil || got != "" || strings.Contains(err.Error(), "DO_NOT_ECHO") {
			t.Fatalf("unsafe remote accepted or exposed: %q, %v", got, err)
		}
	}
}

package gitcredential

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const tokenSentinel = "synthetic-git-token-sentinel"

func valid() Credential {
	return Credential{Name: "brood-gitlab", Repository: "https://git.example.com/group/repo.git", Username: "brood-bot", Token: tokenSentinel}
}

// FEATURE: Server-only Git credentials of a project (#173).
// SPEC: A credential has a lowercase DNS-label name, an exact canonical HTTPS
// repository binding and a visible-ASCII Basic username and token within the
// legacy BROOD_GIT_REMOTES bounds. Refusals name the rule, never the value.
// IMPLEMENTATION: Pure validators shared by store, server, client and CLI.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestValidateCredential(t *testing.T) {
	if err := Validate(valid()); err != nil {
		t.Fatalf("valid credential: %v", err)
	}
	long := valid()
	long.Name = strings.Repeat("a", MaxNameBytes)
	long.Username = strings.Repeat("u", MaxUsernameBytes)
	long.Token = strings.Repeat("t", MaxTokenBytes)
	if err := Validate(long); err != nil {
		t.Fatalf("credential at limits: %v", err)
	}
	for name, mutate := range map[string]func(*Credential){
		"empty name":            func(c *Credential) { c.Name = "" },
		"uppercase name":        func(c *Credential) { c.Name = "Brood" },
		"name with dot":         func(c *Credential) { c.Name = "brood.gitlab" },
		"name with slash":       func(c *Credential) { c.Name = "../brood" },
		"name with edge hyphen": func(c *Credential) { c.Name = "-brood" },
		"long name":             func(c *Credential) { c.Name = strings.Repeat("a", MaxNameBytes+1) },
		"token in repository": func(c *Credential) {
			c.Repository = "https://brood-bot:" + tokenSentinel + "@git.example.com/group/repo.git"
		},
		"query in repository":     func(c *Credential) { c.Repository = "https://git.example.com/group/repo.git?token=" + tokenSentinel },
		"noncanonical repository": func(c *Credential) { c.Repository = "https://Git.Example.com/group/repo.git/" },
		"http repository":         func(c *Credential) { c.Repository = "http://git.example.com/group/repo.git" },
		"ip repository":           func(c *Credential) { c.Repository = "https://192.168.1.10/group/repo.git" },
		"empty repository":        func(c *Credential) { c.Repository = "" },
		"empty username":          func(c *Credential) { c.Username = "" },
		"username with colon":     func(c *Credential) { c.Username = "brood:" + tokenSentinel },
		"username with space":     func(c *Credential) { c.Username = "brood bot" },
		"long username":           func(c *Credential) { c.Username = strings.Repeat("u", MaxUsernameBytes+1) },
		"empty token":             func(c *Credential) { c.Token = "" },
		"token with newline":      func(c *Credential) { c.Token = tokenSentinel + "\n" },
		"token with space":        func(c *Credential) { c.Token = "synthetic " + tokenSentinel },
		"non-ASCII token":         func(c *Credential) { c.Token = tokenSentinel + "é" },
		"long token":              func(c *Credential) { c.Token = strings.Repeat("t", MaxTokenBytes+1) },
	} {
		c := valid()
		mutate(&c)
		err := Validate(c)
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if strings.Contains(err.Error(), tokenSentinel) || strings.Contains(err.Error(), "brood-bot") {
			t.Fatalf("%s: refusal echoes the value: %v", name, err)
		}
	}
}

// formatVerbs covers every verb family with flags and width. String and
// GoString alone serve only %v, %s, %q, %x, %X and %#v; any other verb used to
// print the struct fields, so %d gave {%!d(string=...) ...}. %T prints only
// the type. %p is left out: fmt renders a non-pointer %p by reflection past
// every method, and go vet rejects that call.
var formatVerbs = []string{"%v", "%+v", "%#v", "%s", "%10s", "%-40v", "%q", "%#q", "%x", "%X", "% x", "%d", "%+d", "%5d", "%t", "%c", "%b", "%o", "%O", "%e", "%E", "%f", "%.3g", "%U", "%#U", "%z"}

// SPEC: fmt with any verb and encoding/json never render the username or
// token, including when a credential is nested in another value.
func TestCredentialFormattingRedacts(t *testing.T) {
	c := valid()
	nested := struct {
		Credential Credential
		Items      []Credential
		ByName     map[string]Credential
		Pointer    *Credential
	}{c, []Credential{c}, map[string]Credential{c.Name: c}, &c}
	data, err := json.Marshal(nested)
	if err != nil {
		t.Fatal(err)
	}
	texts := []string{fmt.Sprint(c), fmt.Sprintln(c, &c), string(data)}
	for _, verb := range formatVerbs {
		texts = append(texts, fmt.Sprintf(verb, c), fmt.Sprintf(verb, &c), fmt.Sprintf(verb, nested), fmt.Sprintf(verb, []Credential{c}))
	}
	for _, text := range texts {
		if strings.Contains(text, tokenSentinel) || strings.Contains(text, "brood-bot") {
			t.Fatalf("credential rendered: %s", text)
		}
	}
}

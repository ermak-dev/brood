package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const gitTokenSentinel = "synthetic-git-token-sentinel"

// FEATURE: Installation Git remotes (#146).
// SPEC: BROOD_GIT_REMOTES is a JSON object keyed by canonical HTTPS repository
// URLs; each value is {"username","token"} or {}. Any defect refuses startup
// with the setting name and no submitted values. Credentials are server-only.
// IMPLEMENTATION: Strict token scan without case aliases; redacted types.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestGitRemotesSetting(t *testing.T) {
	value := `{"https://git.example.com/group/repo.git":{"username":"brood-bot","token":"` + gitTokenSentinel + `"},"https://github.com/org/public":{}}`
	for _, raw := range []string{value, "'" + value + "'"} {
		cfg, err := LoadConfig(func(key string) string {
			if key == "BROOD_GIT_REMOTES" {
				return raw
			}
			return ""
		})
		if err != nil {
			t.Fatal(err)
		}
		credential, ok := cfg.gitRemotes["https://git.example.com/group/repo.git"]
		if len(cfg.gitRemotes) != 2 || !ok || credential.username != "brood-bot" || credential.token != gitTokenSentinel || !credential.configured() {
			t.Fatalf("remotes not loaded: %d", len(cfg.gitRemotes))
		}
		if public := cfg.gitRemotes["https://github.com/org/public"]; public.configured() {
			t.Fatal("empty remote reports a credential")
		}
		encoded, _ := json.Marshal(cfg)
		remotesJSON, _ := json.Marshal(cfg.gitRemotes)
		credentialJSON, _ := json.Marshal(credential)
		gateway := newGitGateway(cfg.gitRemotes)
		target := gitTarget{credential: credential}
		for _, text := range []string{
			fmt.Sprintf("%v %+v %#v %s", gateway, gateway, gateway, gateway),
			fmt.Sprintf("%v %+v %#v %s", target, target, target, target),
			fmt.Sprintf("%v %+v %#v %s", cfg, cfg, cfg, cfg),
			fmt.Sprintf("%v %+v %#v %s", cfg.gitRemotes, cfg.gitRemotes, cfg.gitRemotes, cfg.gitRemotes),
			fmt.Sprintf("%v %+v %#v %s", credential, credential, credential, credential),
			string(encoded), string(remotesJSON), string(credentialJSON),
		} {
			if strings.Contains(text, gitTokenSentinel) || strings.Contains(text, "brood-bot") {
				t.Fatalf("credential formatted: %s", text)
			}
		}
	}
	cfg, err := LoadConfig(func(string) string { return "" })
	if err != nil || len(cfg.gitRemotes) != 0 {
		t.Fatalf("absent setting: %v", err)
	}
}

// gitFormatVerbs covers every verb family with flags and width: String and
// GoString serve only %v, %s, %q, %x, %X and %#v, other verbs printed fields.
// %p is left out: fmt renders a non-pointer %p by reflection past every
// method, and go vet rejects that call.
var gitFormatVerbs = []string{"%v", "%+v", "%#v", "%s", "%10s", "%-40v", "%q", "%#q", "%x", "%X", "% x", "%d", "%+d", "%5d", "%t", "%c", "%b", "%o", "%O", "%e", "%E", "%f", "%.3g", "%U", "%#U", "%z"}

// SPEC: Every holder of a Git credential (installation remotes, the config
// that loads them, the gateway, a project access snapshot and a request
// target) hides the username and token for every fmt verb (#173).
func TestGitCredentialHoldersRedactEveryVerb(t *testing.T) {
	origin := "https://git.example.com/group/repo.git"
	cfg, err := LoadConfig(func(key string) string {
		if key == gitRemotesSetting {
			return `{"` + origin + `":{"username":"brood-bot","token":"` + gitTokenSentinel + `"}}`
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	credential := cfg.gitRemotes[origin]
	if credential.token != gitTokenSentinel {
		t.Fatal("remote not loaded")
	}
	access := gitAccess{repository: origin, credential: credential, gateway: true, bound: true}
	gateway := newGitGateway(cfg.gitRemotes)
	for name, value := range map[string]any{
		"credential": credential, "remotes": cfg.gitRemotes, "config": cfg, "config pointer": &cfg,
		"gateway": gateway, "access": access, "target": gitTarget{credential: credential},
	} {
		for _, verb := range gitFormatVerbs {
			if text := fmt.Sprintf(verb, value); strings.Contains(text, gitTokenSentinel) || strings.Contains(text, "brood-bot") {
				t.Errorf("%s with %s rendered: %s", name, verb, text)
				break
			}
		}
	}
}

func TestGitRemotesSettingRefusals(t *testing.T) {
	origin := "https://git.example.com/group/repo.git"
	entry := func(value string) string { return `{"` + origin + `":` + value + `}` }
	credential := `{"username":"brood-bot","token":"` + gitTokenSentinel + `"}`
	many := []string{}
	for i := range 257 {
		many = append(many, fmt.Sprintf(`"https://git.example.com/repo-%d":{}`, i))
	}
	for _, raw := range []string{
		"not-json-" + gitTokenSentinel,
		`[]`, `null`, `"` + gitTokenSentinel + `"`, `{`,
		entry(credential) + ` {}`,
		`{"` + origin + `":{},"` + origin + `":` + credential + `}`,
		`{"` + origin + `":{},"https:\/\/git.example.com\/group\/repo.git":` + credential + `}`,
		`{"https://Git.example.com/group/repo.git":` + credential + `}`,
		`{"https://git.example.com/group/repo.git/":` + credential + `}`,
		`{"https://git.example.com:443/group/repo.git":` + credential + `}`,
		`{"http://git.example.com/group/repo.git":` + credential + `}`,
		`{"https://brood-bot:` + gitTokenSentinel + `@git.example.com/repo":{}}`,
		`{"https://192.168.1.66/repo":{}}`,
		`{"":{}}`,
		entry(`null`), entry(`[]`), entry(`"` + gitTokenSentinel + `"`), entry(`true`),
		entry(`{"username":"brood-bot"}`),
		entry(`{"token":"` + gitTokenSentinel + `"}`),
		entry(`{"username":"","token":"` + gitTokenSentinel + `"}`),
		entry(`{"username":"brood-bot","token":""}`),
		entry(`{"username":"brood-bot","token":"` + gitTokenSentinel + `","password":"x"}`),
		entry(`{"Username":"brood-bot","token":"` + gitTokenSentinel + `"}`),
		entry(`{"username":"brood-bot","Token":"` + gitTokenSentinel + `"}`),
		entry(`{"username":"brood-bot","token":"` + gitTokenSentinel + `","token":"other"}`),
		entry(`{"username":"brood-bot","token":"` + gitTokenSentinel + `","Token":"other"}`),
		entry(`{"username":1,"token":"` + gitTokenSentinel + `"}`),
		entry(`{"username":"brood-bot","token":null}`),
		entry(`{"username":"brood:bot","token":"` + gitTokenSentinel + `"}`),
		entry(`{"username":"brood bot","token":"` + gitTokenSentinel + `"}`),
		entry(`{"username":"brood-bot","token":"` + gitTokenSentinel + ` x"}`),
		entry(`{"username":"brood-bot","token":"` + gitTokenSentinel + `\n"}`),
		entry(`{"username":"brood-bot","token":"` + gitTokenSentinel + `é"}`),
		entry(`{"username":"brood-bot","token":"` + strings.Repeat("t", 4097) + `"}`),
		entry(`{"username":"brood-bot","token":"` + gitTokenSentinel + `","extra":{"nested":true}}`),
		"{" + strings.Join(many, ",") + "}",
	} {
		_, err := LoadConfig(func(key string) string {
			if key == "BROOD_GIT_REMOTES" {
				return raw
			}
			return ""
		})
		var configErr *ConfigError
		if !errors.As(err, &configErr) || configErr.Setting != "BROOD_GIT_REMOTES" {
			t.Fatalf("invalid remotes accepted or unnamed: %v", err)
		}
		if strings.Contains(err.Error(), gitTokenSentinel) || strings.Contains(err.Error(), "brood-bot") || strings.Contains(err.Error(), "git.example.com") {
			t.Fatalf("refusal echoes the setting value: %v", err)
		}
	}
}

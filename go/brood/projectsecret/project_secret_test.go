package projectsecret

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// FEATURE: Bounded, policy-safe project secret bundles (#134).
// SPEC: Validation is pure, refuses runtime controls and ambiguous paths, and never echoes input.
// IMPLEMENTATION: Canonical path and reserved environment checks precede bounded batch validation.
// RESULT: IMPLEMENTED — exact byte/count boundaries and JSON contracts pass.
// STATUS: DONE
func TestValidateNamesAndVars(t *testing.T) {
	for _, name := range []string{".env", "config/app.json", "a-b/_data"} {
		if err := ValidateFileName(name); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"", ".", "..", "/etc/passwd", "../x", "a/../x", "./a", "a//b", "a/", "a\\b", "a\x00b", "C:/x", ".git/config", "x/.git/hooks/post-checkout", ".claude/settings.json", ".claude", ".mcp.json", "CLAUDE.md", "x/CLAUDE.md"} {
		err := ValidateFileName(name)
		if err == nil {
			t.Fatalf("unsafe path accepted at index input length %d", len(name))
		}
		if name != "" && strings.Contains(err.Error(), name) {
			t.Fatal("error echoes invalid name")
		}
	}
	for _, name := range []string{"TOKEN", "_KEY", "App2"} {
		if err := ValidateVar(name, " 'value = x' "); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"", "1KEY", "K-V", "BROOD_TOKEN", "CLAUDE_CONFIG_DIR", "ANTHROPIC_BASE_URL", "HOME", "PWD", "PATH", "USER", "LOGNAME", "SHELL", "LD_PRELOAD", "DYLD_INSERT_LIBRARIES", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "NO_PROXY", "BASH_ENV", "ENV", "PYTHONPATH", "NODE_OPTIONS", "SSL_CERT_FILE"} {
		if err := ValidateVar(name, "SENTINEL"); err == nil || strings.Contains(err.Error(), "SENTINEL") {
			t.Fatal("invalid variable accepted or leaked")
		}
	}
	for _, value := range []string{"a\nb", "a\rb", "a\x00b", strings.Repeat("x", 16*1024+1)} {
		if err := ValidateVar("TOKEN", value); err == nil || strings.Contains(err.Error(), value) {
			t.Fatal("invalid value accepted or leaked")
		}
	}
	if err := ValidateVar("TOKEN", strings.Repeat("x", 16*1024)); err != nil {
		t.Fatal(err)
	}
}

func TestValidateFileNameReservesGitIgnore(t *testing.T) {
	for _, name := range []string{".gitignore", ".GITIGNORE", "nested/.GitIgnore", ".gitignore/key", "nested/.GiTiGnOrE/key"} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateFileName(name); err == nil || strings.Contains(err.Error(), name) {
				t.Fatal("Git ignore component accepted or echoed")
			}
			if err := ValidateBatch([]File{{Name: name, Data: []byte("!*\n")}}, nil); err == nil {
				t.Fatal("Git ignore payload accepted before persistence")
			}
		})
	}
}

func TestValidateBundleBounds(t *testing.T) {
	if err := ValidateBundle(Bundle{}); err != nil {
		t.Fatal(err)
	}
	files := []File{}
	for i := 0; i < 4; i++ {
		files = append(files, File{Name: fmt.Sprintf("f%d", i), Data: make([]byte, 512*1024)})
	}
	if err := ValidateBatch(files, nil); err != nil {
		t.Fatal(err)
	}
	invalid := [][]File{
		{{Name: "big", Data: make([]byte, 512*1024+1)}},
		append(append([]File{}, files...), File{Name: "extra", Data: []byte("x")}),
		{{Name: "a"}, {Name: "a"}}, {{Name: "a"}, {Name: "a/b"}},
	}
	for _, batch := range invalid {
		if ValidateBatch(batch, nil) == nil {
			t.Fatal("invalid files accepted")
		}
	}
	many := []File{}
	vars := map[string]string{}
	for i := 0; i < 128; i++ {
		many = append(many, File{Name: fmt.Sprintf("f%d", i)})
		vars[fmt.Sprintf("V%d", i)] = ""
	}
	if err := ValidateBatch(many, vars); err != nil {
		t.Fatal(err)
	}
	if ValidateBatch(append(many, File{Name: "extra"}), nil) == nil {
		t.Fatal("file count unbounded")
	}
	vars["EXTRA"] = ""
	if ValidateBatch(nil, vars) == nil {
		t.Fatal("var count unbounded")
	}
	vars = map[string]string{}
	for i := 0; i < 4; i++ {
		vars[fmt.Sprintf("V%d", i)] = strings.Repeat("x", 16*1024)
	}
	if ValidateBatch(nil, vars) == nil {
		t.Fatal("environment wire size unbounded")
	}
	vars = map[string]string{"A": strings.Repeat("x", 16*1024), "B": strings.Repeat("x", 16*1024), "C": strings.Repeat("x", 16*1024), "D": strings.Repeat("x", 16*1024-12)}
	if err := ValidateBatch(nil, vars); err != nil {
		t.Fatal("exact environment bound rejected", err)
	}
	vars["D"] += "x"
	if ValidateBatch(nil, vars) == nil {
		t.Fatal("environment byte bound not enforced")
	}
}

func TestSecretJSON(t *testing.T) {
	data, err := json.Marshal(Bundle{Revision: 2, Files: []File{{Name: ".env", Data: []byte("x")}}, Vars: map[string]string{"TOKEN": "v"}})
	if err != nil || string(data) != `{"revision":2,"files":[{"name":".env","data":"eA=="}],"vars":{"TOKEN":"v"}}` {
		t.Fatal("bundle JSON contract")
	}
	data, err = json.Marshal(Metadata{Revision: 2, Files: []FileInfo{{Name: ".env", Size: 1}}, Vars: []string{"TOKEN"}})
	if err != nil || string(data) != `{"revision":2,"files":[{"name":".env","size":1}],"vars":["TOKEN"]}` {
		t.Fatal("metadata JSON contract")
	}
}

package acceptance

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

// FEATURE: Installed bro secret persists opaque project values without a read/export API.
// SPEC: Real CLI/server support safe stdin/files, metadata-only lists and removal; values never enter process output.
func TestRealProjectSecretCommands(t *testing.T) {
	bin := buildBinaries(t)
	f := newInstallation(t, bin)
	f.env = append(f.env, "PATH=/usr/bin:/bin")
	f.cli("secret-state", "admin\n"+f.passwords[0]+"\n", 0, "login", f.origin)
	f.cli("secret-state", "", 0, "project", "add", "secret-project")
	admin := f.browser()
	admin.login("admin", f.passwords[0])
	var projects []project
	decode(t, admin.expect("GET", "/api/projects", nil, 200), &projects)
	if len(projects) != 1 {
		t.Fatal("secret project was not persisted")
	}
	varValue := "synthetic-app-token-for-isolated-acceptance"
	fileValue := "synthetic-file-value-for-isolated-acceptance\n"
	if out := f.cli("secret-state", varValue+"\n", 0, "secret", "--project", "secret-project", "var", "add", "APP_TOKEN", "--stdin"); out != "" {
		t.Fatal("secret mutation printed output")
	}
	out := f.cli("secret-state", "", 0, "secret", "--project", "secret-project", "var", "list")
	if !strings.Contains(out, "APP_TOKEN") || strings.Contains(out, varValue) {
		t.Fatal("variable list omitted name or disclosed value")
	}
	git := exec.Command("git", "init", "--quiet", f.dir)
	git.Env = append([]string(nil), f.env...)
	if output, err := git.CombinedOutput(); err != nil {
		t.Fatalf("isolated git init failed: %s", output)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "fixture.env"), []byte(fileValue), 0600); err != nil {
		t.Fatal(err)
	}
	if out := f.cli("secret-state", "", 0, "secret", "--project", "secret-project", "file", "add", "fixture.env"); out != "" {
		t.Fatal("secret file mutation printed output")
	}
	out = f.cli("secret-state", "", 0, "secret", "--project", "secret-project", "file", "list")
	if !strings.Contains(out, "fixture.env") || strings.Contains(out, strings.TrimSpace(fileValue)) {
		t.Fatal("file list omitted metadata or disclosed value")
	}
	f.stop()
	st, err := store.Open(context.Background(), filepath.Join(f.dir, "brood.db"))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := st.ReadProjectSecretBundle(context.Background(), projects[0].ID)
	if err != nil {
		st.Close()
		t.Fatal(err)
	}
	if bundle.Vars["APP_TOKEN"] != varValue || len(bundle.Files) != 1 || string(bundle.Files[0].Data) != fileValue {
		st.Close()
		t.Fatal("opaque secret values did not persist exactly")
	}
	st.Close()
	f.start()
	f.cli("secret-state", "", 0, "secret", "--project", "secret-project", "file", "remove", "fixture.env")
	f.cli("secret-state", "", 0, "secret", "--project", "secret-project", "var", "remove", "APP_TOKEN")
	if out := f.cli("secret-state", "", 0, "secret", "--project", "secret-project", "var", "list"); strings.Contains(out, "APP_TOKEN") {
		t.Fatal("removed variable stayed visible")
	}
	for _, output := range f.outputs {
		if strings.Contains(output, varValue) || strings.Contains(output, strings.TrimSpace(fileValue)) {
			t.Fatal("secret value appeared in process output")
		}
	}
}

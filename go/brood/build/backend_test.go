// Digest берётся из metadata конкретной сборки, а не общего тега.
// Порт brood/dev/build/tests/test_backend.py: docker подменяется скриптом.
package build

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeDocker ставит в PATH скрипт docker, пишущий metadata и лог вызовов.
type fakeDocker struct {
	t          *testing.T
	dir        string
	exitCode   int
	writeMeta  bool
	metadata   string
	inspect    bool
	callsFile  string
	callsCount int
	once       sync.Once
}

func newFakeDocker(t *testing.T) *fakeDocker {
	t.Helper()
	dir := t.TempDir()
	f := &fakeDocker{t: t, dir: dir, writeMeta: true,
		metadata: "sha256:" + strings.Repeat("a", 64)}
	f.callsFile = filepath.Join(dir, "calls")
	f.install()
	return f
}

func (f *fakeDocker) install() {
	bin := filepath.Join(f.dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	f.rewriteScript()
}

// buildScript собирает подставной docker buildx по текущим полям.
func (f *fakeDocker) buildScript() string {
	writeMeta := fmt.Sprintf(`printf '{"containerimage.digest": "%s"}\n' > "$argument"`, f.metadata)
	if !f.writeMeta {
		writeMeta = ": # no metadata"
	}
	return fmt.Sprintf(`#!/bin/sh
echo build output
echo "$@" >> %[1]s
for argument in "$@"; do
  case "$argument" in
    inspect*) touch %[2]s ;;
  esac
done
if [ -f %[3]s ]; then exit $(cat %[3]s); fi
previous=
for argument in "$@"; do
  if [ "$previous" = "--metadata-file" ]; then
    %[4]s
  fi
  previous=$argument
done
exit 0
`, f.callsFile, filepath.Join(f.dir, "inspected"), filepath.Join(f.dir, "exit"),
		writeMeta)
}

func (f *fakeDocker) calls() []string {
	f.t.Helper()
	data, err := os.ReadFile(f.callsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		f.t.Fatal(err)
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return nil
	}
	return strings.Split(text, "\n")
}

func TestBackendBuildUsesUniqueTagsAndItsOwnMetadata(t *testing.T) {
	f := newFakeDocker(t)
	project := newProjectFixture(t).load(t, "")
	checks := 0
	options := BackendOptions{OwnershipCheck: func() error { checks++; return nil }}
	first, err := BackendBuild(project, options)
	wantNoError(t, err)
	_, err = BackendBuild(project, options)
	wantNoError(t, err)
	want := "localhost:5002/example/python@sha256:" + strings.Repeat("a", 64)
	if first.Image != want {
		t.Fatalf("image: %s", first.Image)
	}
	if first.Stdout == "" {
		t.Fatal("stdout must be captured")
	}
	calls := f.calls()
	if len(calls) < 2 {
		t.Fatalf("expected two docker calls, got %d", len(calls))
	}
	tag := func(call string) string {
		fields := strings.Fields(call)
		for index, field := range fields {
			if field == "--tag" && index+1 < len(fields) {
				return fields[index+1]
			}
		}
		return ""
	}
	if tag(calls[0]) == "" || tag(calls[0]) == tag(calls[1]) {
		t.Fatalf("tags must be unique: %q vs %q", tag(calls[0]), tag(calls[1]))
	}
	for _, call := range calls {
		if strings.Contains(call, "inspect") {
			t.Fatalf("docker inspect is forbidden: %s", call)
		}
	}
	if checks != 2 {
		t.Fatalf("ownership check ran %d times", checks)
	}
}

func TestBackendUnownedRegistryIsRefusedBeforeBuild(t *testing.T) {
	f := newFakeDocker(t)
	project := newProjectFixture(t).load(t, "")
	project.Plan["registry"] = "foreign.example.org/team"
	checked := false
	options := BackendOptions{OwnershipCheck: func() error { checked = true; return nil }}
	_, err := BackendBuild(project, options)
	wantError(t, err, "owned")
	if checked {
		t.Fatal("ownership check must not run for a foreign registry")
	}
	if len(f.calls()) != 0 {
		t.Fatal("docker must not run for a foreign registry")
	}
}

func TestBackendAllowlistAcceptsServerRegistries(t *testing.T) {
	newFakeDocker(t)
	project := newProjectFixture(t).load(t, "")
	project.Plan["registry"] = "reg.example.org:8443/team/app"
	options := BackendOptions{
		OwnershipCheck:    func() error { return nil },
		RegistryAllowlist: []string{"reg.example.org:8443"},
	}
	result, err := BackendBuild(project, options)
	wantNoError(t, err)
	if !strings.HasPrefix(result.Image, "reg.example.org:8443/team/app/example/python@sha256:") {
		t.Fatalf("image: %s", result.Image)
	}
}

func TestBackendMissingMetadataIsNotReplacedWithTagInspection(t *testing.T) {
	f := newFakeDocker(t)
	f.writeMeta = false
	f.rewriteScript()
	project := newProjectFixture(t).load(t, "")
	_, err := BackendBuild(project, BackendOptions{})
	wantError(t, err, "metadata")
	if len(f.calls()) != 1 {
		t.Fatalf("docker must run exactly once, got %d", len(f.calls()))
	}
}

// rewriteScript пересобирает подставной docker после правок полей.
func (f *fakeDocker) rewriteScript() {
	f.t.Helper()
	script := filepath.Join(f.dir, "bin", "docker")
	if err := os.WriteFile(script, []byte(f.buildScript()), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func TestBackendFailedBuildCarriesStreams(t *testing.T) {
	f := newFakeDocker(t)
	if err := os.WriteFile(filepath.Join(f.dir, "exit"), []byte("7"), 0o644); err != nil {
		t.Fatal(err)
	}
	project := newProjectFixture(t).load(t, "")
	_, err := BackendBuild(project, BackendOptions{})
	buildError, ok := err.(*BuildError)
	if !ok {
		t.Fatalf("expected BuildError, got %v", err)
	}
	if buildError.ExitCode != 7 {
		t.Fatalf("exit code: %d", buildError.ExitCode)
	}
	if !strings.Contains(buildError.Error(), "exit code 7") {
		t.Fatalf("message: %s", buildError.Error())
	}
}

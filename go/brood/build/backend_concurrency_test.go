package build

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// VERIFICATION: #18 is already addressed by per-build tags and metadata.
// Two builds overlap at a barrier and must retain their own distinct digests.
func TestConcurrentBackendBuildsKeepTheirOwnDigests(t *testing.T) {
	fake := newFakeDocker(t)
	project := newProjectFixture(t).load(t, "")
	left, right := strings.Repeat("a", 64), strings.Repeat("b", 64)
	script := fmt.Sprintf(`#!/bin/sh
set -eu
metadata=
tag=
previous=
for argument in "$@"; do
  case "$previous" in
    --metadata-file) metadata="$argument" ;;
    --tag) tag="$argument" ;;
  esac
  previous="$argument"
done
printf '%%s\n%%s\n' "$tag" "$metadata" > "$BUILD_TEST_TRACE/$BUILD_TEST_DIGEST.started"
for attempt in $(seq 1 500); do
  if [ -f "$BUILD_TEST_TRACE/%s.started" ] && [ -f "$BUILD_TEST_TRACE/%s.started" ]; then
    printf '{"containerimage.digest":"sha256:%%s"}' "$BUILD_TEST_DIGEST" > "$metadata"
    exit 0
  fi
  sleep 0.01
done
exit 91
`, left, right)
	if err := os.WriteFile(filepath.Join(fake.dir, "bin", "docker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	type outcome struct {
		digest string
		result *BuildResult
		err    error
	}
	done := make(chan outcome, 2)
	for _, digest := range []string{left, right} {
		go func() {
			result, err := BackendBuild(project, BackendOptions{DockerEnv: []string{
				"BUILD_TEST_DIGEST=" + digest, "BUILD_TEST_TRACE=" + fake.dir,
			}})
			done <- outcome{digest, result, err}
		}()
	}
	for range 2 {
		got := <-done
		wantNoError(t, got.err)
		want := "localhost:5002/example/python@sha256:" + got.digest
		if got.result.Image != want {
			t.Fatalf("build %s returned %s, want %s", got.digest, got.result.Image, want)
		}
	}
	traces := make([][]string, 0, 2)
	for _, digest := range []string{left, right} {
		data, err := os.ReadFile(filepath.Join(fake.dir, digest+".started"))
		wantNoError(t, err)
		traces = append(traces, strings.Split(strings.TrimSpace(string(data)), "\n"))
	}
	if len(traces[0]) != 2 || len(traces[1]) != 2 {
		t.Fatalf("missing build arguments: %v", traces)
	}
	for index, field := range []string{"tag", "metadata path"} {
		if traces[0][index] == traces[1][index] {
			t.Fatalf("concurrent builds share %s: %s", field, traces[0][index])
		}
	}
}

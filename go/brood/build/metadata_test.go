package build

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestBackendRejectsNonObjectMetadataWithoutPanic(t *testing.T) {
	for _, text := range []string{"[]", "null", "\"string\"", "42"} {
		t.Run(text, func(t *testing.T) {
			fake := newFakeDocker(t)
			input := filepath.Join(fake.dir, "metadata")
			if err := os.WriteFile(input, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			script := fmt.Sprintf("#!/bin/sh\nprevious=\nfor argument in \"$@\"; do\n if [ \"$previous\" = --metadata-file ]; then cp %q \"$argument\"; fi\n previous=$argument\ndone\n", input)
			if err := os.WriteFile(filepath.Join(fake.dir, "bin", "docker"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			_, err := BackendBuild(newProjectFixture(t).load(t, ""), BackendOptions{})
			wantError(t, err, "metadata")
		})
	}
}

package projectsecret

import (
	"strings"
	"testing"
)

// BUG: Secret filenames are metadata and may be printed; control characters must not reach terminals.
// SPEC: Paths are bounded and free of control characters before storage or delivery.
func TestSecretFilePathRejectsDisplayControlsAndExcessLength(t *testing.T) {
	for _, name := range []string{"file\x1b[31m.env", "file\tname.env", strings.Repeat("a", 1025)} {
		if err := ValidateFileName(name); err == nil {
			t.Fatal("unsafe secret path accepted")
		}
	}
}

//go:build linux

package guest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FEATURE: Root-проекция не понижает malformed ephemeral marker в persistent (#292).
// SPEC: Null и duplicate marker/commit отвергаются так же, как в сетевом receiver.
// IMPLEMENTATION: Root-проекция использует тот же строгий decoder.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestWorkspaceBootstrapProjectionNeverSilentlyDowngradesMalformedMarker(t *testing.T) {
	pin := strings.Repeat("a", 40)
	for _, extra := range []string{
		`"ephemeral":null`,
		fmt.Sprintf(`"ephemeral":{"commit":%q,"commit":%q}`, pin, pin),
		fmt.Sprintf(`"ephemeral":{"commit":%q},"ephemeral":{"commit":%q}`, pin, pin),
	} {
		run := privateTemp(t)
		body := fmt.Sprintf(`{"origin":%q,"mode":"gateway",%s}`, cloneTestOrigin, extra)
		if err := os.WriteFile(filepath.Join(run, workspaceOriginFile), []byte(body), 0440); err != nil {
			t.Fatal(err)
		}
		got, err := readWorkspaceOrigin(run, os.Geteuid(), os.Getegid())
		if err == nil || got != (WorkspaceOrigin{}) {
			t.Fatalf("malformed marker downgraded or accepted: %+v %v", got, err)
		}
	}
}

//go:build linux

package sessionctl

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// BUG: JSON escaping can expand a small complete record past the page budget.
// SPEC: A complete record advances the cursor or emits an explicit notice; pagination never stalls.
func TestHistoryEscapedRecordMakesProgress(t *testing.T) {
	e, path := fixture(t)
	line := `{"type":"assistant","message":{"content":"` + strings.Repeat("<", 5000) + `"}}` + "\n" + `{"type":"user","message":"after"}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0600); err != nil {
		t.Fatal(err)
	}
	first := call(e, "session_history", `{}`)
	var a historyPage
	if !first.OK || json.Unmarshal([]byte(first.Text), &a) != nil {
		t.Fatal(first)
	}
	args, _ := json.Marshal(map[string]any{"cursor": a.Cursor})
	second := call(e, "session_history", string(args))
	var b historyPage
	if !second.OK || json.Unmarshal([]byte(second.Text), &b) != nil {
		t.Fatal(second)
	}
	if len(a.Records) == 0 && a.HasMore && a.Cursor == b.Cursor {
		t.Fatal("complete encoded record permanently stalls history")
	}
	if len(a.Records) == 0 {
		t.Fatal("oversized encoded record has no explicit notice")
	}
}

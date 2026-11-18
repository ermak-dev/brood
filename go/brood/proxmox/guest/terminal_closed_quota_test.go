//go:build linux

package guest

import (
	"fmt"
	"testing"
	"time"
)

// BUG: Закрытые снимки занимают квоту живых терминальных подключений.
// HYPOTHESIS: Квота считает tombstone для replay как активные подключения.
// SOLUTION: Квота учитывает живые TID; отдельный bounded cache сохраняет close replay.
// RESULT: FIXED — 100 закрытых снимков не блокируют owner writer; active quota сохранена.
// STATUS: FIXED
func TestTerminalClosedSnapshotsDoNotConsumeActiveQuota(t *testing.T) {
	f := terminalFixtureNew(t)
	defer f.c.Close()
	for i := 0; i < 100; i++ {
		caller := fmt.Sprintf("snapshot-%d", i)
		a := terminalPage(t, terminalCall(f.c, caller, `{"action":"open","mode":"ro"}`))
		req := fmt.Sprintf(`{"action":"close","tid":%q,"seq":1}`, a.TID)
		closed := terminalCall(f.c, caller, req)
		if !closed.OK || terminalCall(f.c, caller, req) != closed {
			t.Fatal("close replay lost", closed)
		}
		f.now = f.now.Add(time.Millisecond)
	}
	for i := 0; i < 3; i++ {
		terminalPage(t, terminalCall(f.c, "viewer", `{"action":"open","mode":"ro"}`))
	}
	writer := terminalPage(t, terminalCall(f.c, "owner", `{"action":"open","mode":"rw"}`))
	if terminalCall(f.c, "extra", `{"action":"open","mode":"ro"}`).OK {
		t.Fatal("active attachment quota lost")
	}
	before := len(f.steps)
	if terminalCall(f.c, "owner", fmt.Sprintf(`{"action":"input","tid":%q,"seq":1,"data":"owner input"}`, writer.TID)).OK == false || len(f.steps) != before+1 {
		t.Fatal("owner writer unavailable")
	}
	if len(f.c.attachments) > 64 {
		t.Fatal("closed metadata is unbounded", len(f.c.attachments))
	}
}

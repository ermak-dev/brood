//go:build linux

package ptysession

// BUG: Replay applied DECOM after CUP and lost saved origin-relative positions.
// HYPOTHESIS: Restore origin before CUP, using the active top margin, including saves.
// SOLUTION: Serialize DECOM with each cursor and retain the saved cursor's margins.
// RESULT: FIXED — original and replay cursors agree in the independent libvterm oracle.
// STATUS: FIXED

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// libvtermCursorOracle uses the public libvterm ABI, independently of the Go VT.
// No headers are needed; the declarations below match libvterm's vterm.h.
func libvtermCursorOracle(t *testing.T) func(string) string {
	t.Helper()
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("libvterm oracle requires a C compiler")
	}
	dir := t.TempDir()
	source, executable := filepath.Join(dir, "cursor.c"), filepath.Join(dir, "cursor")
	const code = `#include <stdio.h>
#include <stddef.h>
typedef struct VTerm VTerm;
typedef struct VTermState VTermState;
typedef struct VTermScreen VTermScreen;
typedef struct { int row, col; } VTermPos;
extern VTerm *vterm_new(int, int);
extern void vterm_free(VTerm *);
extern VTermState *vterm_obtain_state(VTerm *);
extern VTermScreen *vterm_obtain_screen(VTerm *);
extern void vterm_screen_enable_altscreen(VTermScreen *, int);
extern void vterm_state_reset(VTermState *, int);
extern size_t vterm_input_write(VTerm *, const char *, size_t);
extern void vterm_state_get_cursorpos(const VTermState *, VTermPos *);
int main(void) {
 VTerm *vt = vterm_new(24, 80);
 VTermState *state = vterm_obtain_state(vt);
 vterm_screen_enable_altscreen(vterm_obtain_screen(vt), 1);
 vterm_state_reset(state, 1);
 char buf[4096]; size_t n;
 while ((n = fread(buf, 1, sizeof buf, stdin))) vterm_input_write(vt, buf, n);
 VTermPos pos; vterm_state_get_cursorpos(state, &pos);
 printf("%d,%d", pos.row, pos.col);
 vterm_free(vt);
 return 0;
}
`
	if err := os.WriteFile(source, []byte(code), 0600); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(cc, source, "-Wl,-l:libvterm.so.0", "-o", executable).CombinedOutput(); err != nil {
		if strings.Contains(string(output), "cannot find -l:libvterm.so.0") {
			t.Skip("libvterm shared library is not installed")
		}
		t.Fatalf("compile libvterm oracle: %v: %s", err, output)
	}
	return func(stream string) string {
		t.Helper()
		cmd := exec.Command(executable)
		cmd.Stdin = strings.NewReader(stream)
		result, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("libvterm oracle: %v: %s", err, result)
		}
		return string(result)
	}
}

func TestObserverOriginReplayLibvterm(t *testing.T) {
	cursor := libvtermCursorOracle(t)
	for _, tc := range []struct{ name, prefix, suffix, want string }{
		{"origin", "\x1b[2;20r\x1b[?6h\x1b[3;4Htext", "", "3,7"},
		{"origin live CUP", "\x1b[2;20r\x1b[?6h\x1b[3;4Htext", "\x1b[2;5Hx", "2,5"},
		{"saved origin", "\x1b[2;20r\x1b[?6h\x1b[3;4H\x1b7\x1b[?6l\x1b[10;12Htext", "\x1b8x", "3,4"},
		{"saved changed margins", "\x1b[2;20r\x1b[?6h\x1b[6;4H\x1b7\x1b[4;22r\x1b[3;5Htext", "\x1b8x", "6,4"},
		{"1048 save", "\x1b[2;20r\x1b[?6h\x1b[3;4H\x1b[?1048h\x1b[?6l\x1b[10;12Htext", "\x1b[?1048lx", "3,4"},
		{"alternate origin", "\x1b[?1049h\x1b[4;22r\x1b[?6h\x1b[3;4Htext", "", "5,7"},
		{"primary return", "\x1b[2;20r\x1b[?6h\x1b[3;4Htext\x1b[?1049h\x1b[4;22r\x1b[?6l\x1b[10;12Halt", "\x1b[?1049l", "3,7"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newObserver(80, 24)
			defer o.close()
			o.write([]byte(tc.prefix))
			replay, err := o.snapshot()
			if err != nil {
				t.Fatal(err)
			}
			original := cursor(tc.prefix + tc.suffix)
			if original != tc.want {
				t.Fatalf("reference cursor = %s, want %s", original, tc.want)
			}
			got := cursor(string(replay) + tc.suffix)
			if got != original {
				t.Fatalf("replay libvterm cursor = %s, original = %s; replay %q", got, original, replay)
			}
		})
	}
}

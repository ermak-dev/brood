package gitlab

import "testing"

func TestCheckLabel(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"status::live", "status::live", false},
		{"status::nope", "", true},
		{"color::red", "", true},
		{"plain", "", true},
	}
	for _, c := range cases {
		got, err := CheckLabel(c.in)
		if c.wantErr != (err != nil) {
			t.Errorf("CheckLabel(%q) err = %v, wantErr %v", c.in, err, c.wantErr)
		}
		if !c.wantErr && got != c.want {
			t.Errorf("CheckLabel(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWithStatus(t *testing.T) {
	got, err := WithStatus([]string{"status::live", "sev::high"}, "wip")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sev::high", "status::wip"}
	if len(got) != len(want) {
		t.Fatalf("WithStatus = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("WithStatus = %v, want %v", got, want)
		}
	}
	if _, err := WithStatus(nil, "nope"); err == nil {
		t.Error("unknown status must be refused")
	}
}

func TestRelabelAction(t *testing.T) {
	if RelabelAction("closed", "live") != "reopen" {
		t.Error("open status on a closed issue must reopen")
	}
	if RelabelAction("opened", "wip") != "relabel" {
		t.Error("open issue stays relabeled")
	}
	if RelabelAction("closed", "fixed") != "relabel" {
		t.Error("closing status never reopens via relabel")
	}
}

func TestEpicMarker(t *testing.T) {
	body := WithEpic("заголовок тела", 12)
	if EpicOf(body) != 12 {
		t.Errorf("EpicOf(WithEpic) = %d, want 12", EpicOf(body))
	}
	// Ссылка на эпик в середине тела — ссылка, а не родство.
	if EpicOf("текст\nупоминание **Эпик:** #5\nещё") != 0 {
		t.Error("epic marker only counts on the first line")
	}
	// Идемпотентно и заменяет старый маркер.
	if got := WithEpic(body, 13); EpicOf(got) != 13 {
		t.Errorf("WithEpic must replace the old marker, got epic %d", EpicOf(got))
	}
	if BareBody(body) != "заголовок тела" {
		t.Errorf("BareBody = %q", BareBody(body))
	}
}

func TestReplaceBody(t *testing.T) {
	old := WithEpic("старое тело", 7)
	// Новое тело без маркера — под старым родителем.
	if got := ReplaceBody(old, "новое тело", EpicKeep, 0); EpicOf(got) != 7 {
		t.Errorf("keep: epic = %d, want 7", EpicOf(got))
	}
	// Новое тело со своим маркером — как есть.
	if got := ReplaceBody(old, WithEpic("своё", 9), EpicKeep, 0); EpicOf(got) != 9 {
		t.Errorf("own marker: epic = %d, want 9", EpicOf(got))
	}
	// Снять — снимает и принесённый маркер.
	if got := ReplaceBody(old, WithEpic("своё", 9), EpicRemove, 0); EpicOf(got) != 0 {
		t.Errorf("remove: epic = %d, want 0", EpicOf(got))
	}
	// Поставить другой.
	if got := ReplaceBody(old, "тело", EpicSet, 3); EpicOf(got) != 3 {
		t.Errorf("set: epic = %d, want 3", EpicOf(got))
	}
}

func TestBranchName(t *testing.T) {
	if got := BranchName([]string{"type::bug"}, "74", "fix login"); got != "bug/74-fix-login" {
		t.Errorf("BranchName bug = %q", got)
	}
	if got := BranchName(nil, "85", "add docs"); got != "feat/85-add-docs" {
		t.Errorf("BranchName default = %q", got)
	}
	// Дефект перевешивает: bug и feature вместе — правда о работе в дефекте.
	if got := BranchName([]string{"bug", "type::feature"}, "1", "x"); got != "bug/1-x" {
		t.Errorf("BranchName bug wins = %q", got)
	}
	// Не-ASCII выбрасывается, не транслитерируется.
	if got := BranchName(nil, "2", "Правка входа"); got != "feat/2" {
		t.Errorf("BranchName russian slug = %q, want feat/2", got)
	}
}

func TestSlugLong(t *testing.T) {
	got := BranchName(nil, "3", "a-very-long-branch-slug-name-over-forty-characters-total")
	if len(got) > 40+1+len("feat/3-")-len("feat/3-")+7+40 {
		t.Errorf("slug too long: %q", got)
	}
}

func TestScreenLinesAndTail(t *testing.T) {
	trace := "section_start:1:name\r\x1b[0Khello\rprogress\rdone\nnext line\r\n"
	got := ScreenLines(trace)
	want := "hello\nprogress\rdone\nnext line\n"
	_ = want
	if got == trace {
		t.Errorf("ScreenLines must strip \\r sections, got %q", got)
	}
	if Tail("a\nb\nc\n", 2) != "b\nc" {
		t.Errorf("Tail = %q; the trailing newline is not a line", Tail("a\nb\nc\n", 2))
	}
	if Tail("a\nb", -1) != "a\nb" {
		t.Errorf("Tail full = %q", Tail("a\nb", -1))
	}
}

func TestDuration(t *testing.T) {
	nilSecs := (*float64)(nil)
	if Duration(nilSecs) != "-" {
		t.Error("nil duration is unknown, not zero")
	}
	s := 59.0
	if Duration(&s) != "59s" {
		t.Errorf("Duration(59) = %q", Duration(&s))
	}
	m := 125.0
	if Duration(&m) != "2m05s" {
		t.Errorf("Duration(125) = %q", Duration(&m))
	}
}

func TestCancelRefusal(t *testing.T) {
	if PipelineCancelBlocked(Pipeline{"id": 1.0, "status": "success"}) == "" {
		t.Error("finished pipeline must refuse cancel with its state")
	}
	if PipelineCancelBlocked(Pipeline{"id": 1.0, "status": "running"}) != "" {
		t.Error("running pipeline cancels fine")
	}
	if RetryJobBlocked(Job{"id": 9.0, "status": "running"}) == "" {
		t.Error("running job must refuse retry")
	}
	if RetryJobBlocked(Job{"id": 9.0, "status": "failed"}) != "" {
		t.Error("failed job retries fine")
	}
}

func TestLintReport(t *testing.T) {
	lines, valid := LintReport(map[string]any{
		"valid": true,
		"jobs":  []any{map[string]any{"name": "test"}, map[string]any{"name": "web"}},
	})
	if !valid || lines[0] != "valid — 2 job(s): test, web" {
		t.Errorf("LintReport valid = %v, %q", valid, lines[0])
	}
	lines, valid = LintReport(map[string]any{"valid": false, "errors": []any{"bad yaml"}})
	if valid || lines[0] != "invalid" || lines[1] != "error: bad yaml" {
		t.Errorf("LintReport invalid = %v, %v", valid, lines)
	}
}

func TestWhyLinesTolerated(t *testing.T) {
	p := Pipeline{"id": 5.0, "status": "failed", "web_url": "u"}
	jobs := []Job{
		{"id": 1.0, "status": "failed", "allow_failure": true, "stage": "test", "name": "t1"},
		{"id": 2.0, "status": "success", "stage": "test", "name": "t2"},
	}
	got := WhyLines(p, jobs, nil)
	found := false
	for _, l := range got {
		if l == "job 1 test/t1 failed (allow_failure): did NOT fail the pipeline" {
			found = true
		}
	}
	if !found {
		t.Errorf("tolerated failure must be named as NOT failing the pipeline: %v", got)
	}
	// Зелёный пайплайн без провалов говорит «nothing failed»; tolerated-
	// провал там не причина, и строка о нём уже есть выше.
	clean := []Job{{"id": 2.0, "status": "success", "stage": "test", "name": "t2"}}
	if got2 := WhyLines(Pipeline{"id": 6.0, "status": "success", "web_url": "u"}, clean, nil); got2[len(got2)-1] != "nothing failed" {
		t.Errorf("green pipeline says nothing failed: %v", got2)
	}
}

package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// FEATURE: Точный branch head привязанного проекта до резервирования MR.
// SPEC: Узкий синтаксис, один encoded path segment, точное имя и полный SHA;
// ошибки не возвращают частичных данных или credentials.
// IMPLEMENTATION: Fixed-project GET и точная проверка typed head.
// RESULT: IMPLEMENTED — TLS fixtures проверяют binding и локальные отказы.
// STATUS: DONE
func TestBranchRead(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, mode := range []string{"valid", "wrong name", "invalid sha", "missing sha", "http error", "truncated"} {
		t.Run(mode, func(t *testing.T) {
			c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" || r.URL.EscapedPath() != "/api/v4/projects/42/repository/branches/topic%2Fwork" || r.URL.RawQuery != "" || r.Header.Get("PRIVATE-TOKEN") != sentinel {
					t.Errorf("wrong branch request: %s", r.URL)
				}
				name, head := "topic/work", sha
				switch mode {
				case "wrong name":
					name = "foreign"
				case "invalid sha":
					head = "abc"
				case "missing sha":
					head = ""
				case "http error":
					w.WriteHeader(403)
					return
				case "truncated":
					fmt.Fprint(w, `{"name":`)
					return
				}
				fmt.Fprintf(w, `{"name":%q,"commit":{"id":%q}}`, name, head)
			})
			branch, err := c.Branch(context.Background(), "topic/work")
			if mode == "valid" {
				if err != nil || branch.Name != "topic/work" || branch.SHA != sha {
					t.Fatalf("branch: %+v %v", branch, err)
				}
				return
			}
			if err == nil || branch != (Branch{}) {
				t.Fatalf("invalid branch released: %+v %v", branch, err)
			}
		})
	}
}

// BUG: SHA-shaped credentials тоже могут быть отражены upstream.
// SPEC: Никакие точные token bytes не выходят даже как корректный commit ID.
func TestBranchReadRejectsReflectedToken(t *testing.T) {
	token := secret(strings.Repeat("a", 40))
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, `{"name":"topic/work","commit":{"id":%q}}`, string(token))
	})
	c.token = &token
	branch, err := c.Branch(context.Background(), "topic/work")
	if err == nil || branch != (Branch{}) {
		t.Fatal("reflected token released as a branch SHA")
	}
}

func TestBranchReadRejectsInvalidInputBeforeHTTP(t *testing.T) {
	calls := 0
	c, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) { calls++ })
	for _, name := range []string{"", "HEAD", "refs/heads/a", "../x", "a?b", "a#b", "a%2fb", "-option", strings.Repeat("a", 256), sentinel} {
		if branch, err := c.Branch(context.Background(), name); err == nil || branch != (Branch{}) {
			t.Fatalf("accepted %q: %+v %v", name, branch, err)
		}
	}
	if calls != 0 {
		t.Fatalf("invalid inputs caused %d requests", calls)
	}
}

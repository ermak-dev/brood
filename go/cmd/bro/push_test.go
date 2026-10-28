package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/build"
	"brood.dev/go/brood/clientapi"
)

// FEATURE: отдельная публикация готового комплекта через сохранённую сессию.
// SPEC: push имеет собственную справку, требует login, не запускает сборку.
func TestPushPreviouslyBuiltArchiveWithoutDocker(t *testing.T) {
	isolateState(t)
	root := gitProject(t)
	archive, digest := cliOCIArchive(t)
	selected, err := build.LoadWorkingProject(root, "development", "example")
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := build.Render(selected.Plan, "brood.local/example/python@"+digest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := build.PublishLocal(root, "development", artifacts, archive); err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		ck, err := r.Cookie("session")
		if err != nil || ck.Value != "session-secret" {
			t.Error("missing session")
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Header.Get("X-CSRFToken") != "csrf-value" {
			t.Error("missing csrf")
		}
		switch {
		case r.URL.Path == "/api/auth/session":
			fmt.Fprint(w, `{"authenticated":true,"user":{"username":"alice"}}`)
		case r.URL.Path == "/api/projects":
			fmt.Fprint(w, `[{"id":"p1","name":"example"}]`)
		case r.URL.Path == "/api/projects/p1/swarms":
			fmt.Fprint(w, `[{"id":"s1","name":"personal","state":"active"}]`)
		case r.Method == "HEAD":
			w.WriteHeader(404)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/uploads"):
			w.WriteHeader(201)
			fmt.Fprint(w, `{"upload_id":"upload-1"}`)
		case r.Method == "PATCH":
			data, _ := io.ReadAll(r.Body)
			if len(data) == 0 {
				t.Error("empty chunk")
			}
			w.WriteHeader(204)
		case r.Method == "PUT" && strings.Contains(r.URL.Path, "/uploads/"):
			if r.URL.Query().Get("digest") == "" {
				t.Error("missing digest")
			}
			w.WriteHeader(204)
		case r.Method == "PUT" && strings.Contains(r.URL.Path, "/manifests/"):
			fmt.Fprintf(w, `{"image":"localhost:5002/swarm-s1/python@%s"}`, digest)
		case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/templates"):
			var body map[string]json.RawMessage
			if json.NewDecoder(r.Body).Decode(&body) != nil || len(body["actor_template"]) == 0 || len(body["egress_policy"]) == 0 {
				t.Error("missing template bundle")
			}
			var actor struct {
				Containers []struct {
					Image string `json:"image"`
				} `json:"containers"`
			}
			if json.Unmarshal(body["actor_template"], &actor) != nil || len(actor.Containers) != 1 || actor.Containers[0].Image != "localhost:5002/swarm-s1/python@"+digest {
				t.Error("template image was not rebound to the server repository")
				w.WriteHeader(400)
				return
			}
			fmt.Fprintf(w, `{"image":"localhost:5002/swarm-s1/python@%s","template":"example-development","project":"s1"}`, digest)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	store, err := clientapi.NewSessionStore(filepath.Join(os.Getenv("XDG_STATE_HOME"), "brood", "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(server.URL, clientapi.StoredSession{Session: "session-secret", CSRF: "csrf-value"}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCurrent(server.URL); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	if err := os.WriteFile(filepath.Join(root, "Broodfile"), []byte("changed after build"), 0600); err != nil {
		t.Fatal(err)
	}
	log := fakeDocker(t, "exit 99\n")
	code, out, diagnostic := invoke(t, "", "push", "development", "--project", "example")
	if code != 0 || !strings.Contains(out, "example-development") || diagnostic != "" {
		t.Fatalf("push %d %q %q requests=%v", code, out, diagnostic, calls)
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatal("push rebuilt image")
	}
}

func TestPushHelpAndNoSession(t *testing.T) {
	isolateState(t)
	code, out, diagnostic := invoke(t, "", "push", "--help")
	if code != 0 || diagnostic != "" || !strings.Contains(out, "--project") || strings.Contains(out, "--atespace") {
		t.Fatalf("help: %d %q %q", code, out, diagnostic)
	}
	log := fakeDocker(t, "exit 99\n")
	defer func() {
		if _, err := os.Stat(log); !os.IsNotExist(err) {
			t.Errorf("push invoked Docker: %v", err)
		}
	}()
	code, _, diagnostic = invoke(t, "", "push", "development", "--project", "example")
	if code == 0 || !strings.Contains(diagnostic, "bro login") {
		t.Fatalf("missing login: %d %q", code, diagnostic)
	}
}

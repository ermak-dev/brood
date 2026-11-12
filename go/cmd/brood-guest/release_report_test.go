//go:build linux

package main

// FEATURE: The guest root reports its current guest release (#133).
// SPEC: Answering relay.GuestReleaseOperation, the supervisor reads only the
// bin entry of its artifact root, without download or hashing: a link to
// releases/<id> is layout release with that id, the plain bin directory of a
// bootstrap before #184 is layout plain, and anything else (no root, no bin,
// a foreign or malformed link, a root that is a link or writable by others)
// fails with a static reason and no guess. Every other session operation
// reaches the supervisor handler unchanged.
// IMPLEMENTATION: reportRelease on a temporary artifact root built by the real
// installRelease; releaseSessions wraps the supervisor handler.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/proxmox/relay"
)

func expectReport(t *testing.T, got relay.SessionResponse, want relay.GuestRelease) {
	t.Helper()
	if !got.OK || got.Outcome != "complete" {
		t.Fatalf("release report: %+v, want %+v", got, want)
	}
	var answer relay.GuestRelease
	if err := json.Unmarshal([]byte(got.Text), &answer); err != nil || answer != want {
		t.Fatalf("release report %q (%v), want %+v", got.Text, err, want)
	}
}

func TestReportReleaseNamesLinkedRelease(t *testing.T) {
	root := newRoot(t)
	a, b := newReleaseSet("a"), newReleaseSet("b")
	mustStart(t, root, &fakeServer{set: a})
	expectReport(t, reportRelease(root), relay.GuestRelease{Layout: "release", Release: a.id(t)})
	// Следующий старт с другим набором: отчёт называет новый bin, не previous.
	mustStart(t, root, &fakeServer{set: b})
	expectReport(t, reportRelease(root), relay.GuestRelease{Layout: "release", Release: b.id(t)})
}

func TestReportReleasePlainBin(t *testing.T) {
	root := newRoot(t)
	mkdir(t, root)
	writeSet(t, filepath.Join(root, "bin"), newReleaseSet("old"))
	expectReport(t, reportRelease(root), relay.GuestRelease{Layout: "plain"})
}

func TestReportReleaseRefusesToGuess(t *testing.T) {
	a := newReleaseSet("a")
	cases := map[string]func(t *testing.T, root, outside string){
		"no root": func(*testing.T, string, string) {},
		"no bin":  func(t *testing.T, root, _ string) { mkdir(t, root) },
		"bin is a file": func(t *testing.T, root, _ string) {
			mkdir(t, root)
			writeFile(t, filepath.Join(root, "bin"), "plain file")
		},
		"foreign link": func(t *testing.T, root, outside string) {
			mkdir(t, root)
			writeSet(t, outside, a)
			symlink(t, outside, filepath.Join(root, "bin"))
		},
		"malformed release link": func(t *testing.T, root, _ string) {
			mkdir(t, root)
			symlink(t, "releases/"+strings.Repeat("A", 64), filepath.Join(root, "bin"))
		},
		"root is a link": func(t *testing.T, root, outside string) {
			installed := newRoot(t)
			mustStart(t, installed, &fakeServer{set: a})
			symlink(t, installed, root)
		},
		"root writable by others": func(t *testing.T, root, _ string) {
			mustStart(t, root, &fakeServer{set: a})
			chmod(t, root, 0o777)
		},
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			root, outside := newRoot(t), t.TempDir()
			prepare(t, root, outside)
			got := reportRelease(root)
			if got.OK || got.Outcome != "failed" || got.Text != "guest release is not readable" {
				t.Fatalf("unreadable release reported: %+v", got)
			}
		})
	}
}

func TestReleaseSessionsAnswerBeforeSupervisor(t *testing.T) {
	root := newRoot(t)
	a := newReleaseSet("a")
	mustStart(t, root, &fakeServer{set: a})
	var passed []string
	handler := releaseSessions(root, func(_ context.Context, req relay.SessionRequest) relay.SessionResponse {
		passed = append(passed, req.Operation)
		return relay.SessionResponse{OK: true, Text: "supervisor", Outcome: "complete"}
	})
	expectReport(t, handler(context.Background(), relay.SessionRequest{Operation: relay.GuestReleaseOperation, Arguments: json.RawMessage(`{}`), Caller: "server"}), relay.GuestRelease{Layout: "release", Release: a.id(t)})
	for _, operation := range []string{"tail", "restart", relay.WorkspaceAttestOperation} {
		if got := handler(context.Background(), relay.SessionRequest{Operation: operation, Arguments: json.RawMessage(`{}`), Caller: "server"}); got.Text != "supervisor" {
			t.Fatalf("%s did not reach the supervisor: %+v", operation, got)
		}
	}
	if strings.Join(passed, ",") != "tail,restart,"+relay.WorkspaceAttestOperation {
		t.Fatalf("supervisor calls: %v", passed)
	}
}

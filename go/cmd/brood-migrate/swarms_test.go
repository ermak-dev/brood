// Проверки точного связывания и атомарности офлайн-импорта роёв.
package main

import (
	"path/filepath"
	"strings"
	"testing"

	"brood.dev/go/brood/store"
)

func goDB(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "go.db")
	st, err := store.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, u := range []store.User{{ID: "alice-id", Username: "alice", PasswordHash: "unused", Disabled: true}, {ID: "bob-id", Username: "bob", PasswordHash: "unused"}} {
		if err := st.CreateUser(t.Context(), u); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.CreateProject(t.Context(), store.Project{ID: "project-id", Name: "main"}); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSwarmsExactRerunAndDisabledOwnerMembership(t *testing.T) {
	target := goDB(t, t.TempDir())
	mapping := mappingFixture(t, `{"space":{"project":"main","owner":"alice","uid":"uid-1"}}`)
	verified := func(string) (string, error) { return "uid-1", nil }
	for range 2 {
		if err := migrateSwarms(mapping, target, verified); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := store.Open(t.Context(), target)
	defer st.Close()
	sw, err := st.GetSwarmByAtespace(t.Context(), "space")
	if err != nil {
		t.Fatal(err)
	}
	if sw.OwnerID != "alice-id" || sw.ProjectID != "project-id" || sw.AtespaceUID == nil || *sw.AtespaceUID != "uid-1" {
		t.Fatalf("wrong link: %+v", sw)
	}
	member, err := st.IsMember(t.Context(), "project-id", "alice-id")
	if err != nil || !member {
		t.Fatal("disabled owner lacks exact membership")
	}
	u, _ := st.GetUser(t.Context(), "alice-id")
	if !u.Disabled {
		t.Fatal("owner was enabled")
	}
}

func TestSwarmsRejectMovesMissingOrUnverifiedUID(t *testing.T) {
	target := goDB(t, t.TempDir())
	verified := func(string) (string, error) { return "uid-1", nil }
	good := mappingFixture(t, `{"space":{"project":"main","owner":"alice","uid":"uid-1"}}`)
	if err := migrateSwarms(good, target, verified); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"space":{"project":"main","owner":"bob","uid":"uid-1"}}`,
		`{"space":{"project":"main","owner":"alice","uid":"uid-2"}}`,
		`{"space":{"project":"main","owner":"alice"}}`,
		`{"new":{"project":"main","owner":"alice","uid":"uid-2"}}`,
		`{"space":{"project":"main","owner":"alice","uid":"uid-1"},"space":{"project":"main","owner":"bob","uid":"uid-1"}}`,
	} {
		if err := migrateSwarms(mappingFixture(t, body), target, verified); err == nil {
			t.Fatalf("unsafe mapping accepted: %s", body)
		}
	}
	if err := migrateSwarms(good, target, nil); err == nil {
		t.Fatal("unverified UID accepted")
	}
}

func TestSwarmsFailureRollsBackMembershipAndLinks(t *testing.T) {
	target := goDB(t, t.TempDir())
	db := openSQL(t, target)
	mustExec(t, db, `CREATE TRIGGER reject_z BEFORE INSERT ON swarms WHEN NEW.atespace_name='z' BEGIN SELECT RAISE(ABORT,'injected'); END`)
	db.Close()
	mapping := mappingFixture(t, `{"a":{"project":"main","owner":"alice","uid":"uid-a"},"z":{"project":"main","owner":"bob","uid":"uid-z"}}`)
	if err := migrateSwarms(mapping, target, func(name string) (string, error) { return "uid-" + name, nil }); err == nil {
		t.Fatal("failure accepted")
	}
	db = openSQL(t, target)
	defer db.Close()
	assertCount(t, db, "swarms", 0)
	assertCount(t, db, "project_members", 0)
}

func TestSwarmsRejectForeignProjectAndUIDReuse(t *testing.T) {
	target := goDB(t, t.TempDir())
	st, err := store.Open(t.Context(), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateProject(t.Context(), store.Project{ID: "other-project", Name: "other"}); err != nil {
		t.Fatal(err)
	}
	st.Close()
	verified := func(string) (string, error) { return "uid-1", nil }
	original := mappingFixture(t, `{"space":{"project":"main","owner":"alice","uid":"uid-1"}}`)
	if err := migrateSwarms(original, target, verified); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{"space":{"project":"other","owner":"alice","uid":"uid-1"}}`,
		`{"new-name":{"project":"main","owner":"alice","uid":"uid-1"}}`,
		`{"space":{"project":"main","owner":"alice","uid":"uid-2"}}`,
		`{"new":{"project":"missing","owner":"alice","uid":"uid-1"}}`,
		`{"new":{"project":"main","owner":"missing","uid":"uid-1"}}`,
	} {
		uid := "uid-1"
		if strings.Contains(body, "uid-2") {
			uid = "uid-2"
		}
		if err := migrateSwarms(mappingFixture(t, body), target, func(string) (string, error) { return uid, nil }); err == nil {
			t.Fatalf("conflicting link accepted: %s", body)
		}
	}
	db := openSQL(t, target)
	defer db.Close()
	assertCount(t, db, "swarms", 1)
	assertCount(t, db, "project_members", 1)
}

func TestSnapshotVerification(t *testing.T) {
	snapshot := mappingFixture(t, `{"items":[{"metadata":{"name":"space","uid":"uid-1"}}]}`)
	mapping := mappingFixture(t, `{"space":{"project":"main","owner":"alice","uid":"uid-1"}}`)
	if err := runSwarms(mapping, goDB(t, t.TempDir()), snapshot); err != nil {
		t.Fatal(err)
	}
}

package store

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"brood.dev/go/brood/gitcredential"
	"brood.dev/go/brood/projectsecret"
)

const (
	gitCredentialRepository = "https://git.example.com/group/repo.git"
	gitCredentialToken      = "synthetic-git-token-sentinel"
)

func testGitCredential(name, repository, token string) gitcredential.Credential {
	return gitcredential.Credential{Name: name, Repository: repository, Username: "brood-bot", Token: token}
}

// FEATURE: Server-only project Git credentials in the private database (#173).
// SPEC: Credentials live apart from delivered var/file secrets; listing is
// metadata only; the gateway credential resolves only when the project
// declaration references it and its bound repository equals the project
// repository; rotation replaces it, removal and unbinding revoke it; project
// deletion cascades; the state survives reopen.
// IMPLEMENTATION: Separate tables, one-statement access snapshot.
// RESULT: IMPLEMENTED
// STATUS: DONE
func TestProjectGitCredentialAccess(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "git.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { s.Close() }()
	seedProject(t, s, "p", "p")
	seedProject(t, s, "q", "q")
	access := func(project string) ProjectGitAccess {
		t.Helper()
		got, err := s.ProjectGitAccess(ctx, project)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := access("p"); got.Repository != "" || got.HasCredential() {
		t.Fatalf("empty project access: %v %v", got.Repository, got.HasCredential())
	}
	if _, err := s.ProjectGitAccess(ctx, "missing"); !isNotFound(err) {
		t.Fatalf("missing project access: %v", err)
	}
	if err := s.PutProjectGitCredential(ctx, "p", testGitCredential("gitlab", gitCredentialRepository, gitCredentialToken)); err != nil {
		t.Fatal(err)
	}
	if err := s.PutProjectGitCredential(ctx, "p", testGitCredential("spare", "https://git.example.com/group/other.git", "synthetic-spare-token")); err != nil {
		t.Fatal(err)
	}
	// A stored credential is inert until the repository and reference agree.
	if got := access("p"); got.HasCredential() {
		t.Fatal("unreferenced credential resolved")
	}
	if err := s.SetProjectRepository(ctx, "p", gitCredentialRepository); err != nil {
		t.Fatal(err)
	}
	if got := access("p"); got.HasCredential() {
		t.Fatal("credential resolved without a declaration reference")
	}
	if err := s.SetProjectGitCredentialRef(ctx, "p", "gitlab"); err != nil {
		t.Fatal(err)
	}
	got := access("p")
	if got.Repository != gitCredentialRepository || !got.HasCredential() || got.Credential.Name != "gitlab" || got.Credential.Username != "brood-bot" || got.Credential.Token != gitCredentialToken || got.Credential.Repository != gitCredentialRepository {
		t.Fatalf("referenced credential did not resolve: %v", got.HasCredential())
	}
	infos, err := s.ProjectGitCredentials(ctx, "p")
	want := []gitcredential.Info{{Name: "gitlab", Repository: gitCredentialRepository, Active: true}, {Name: "spare", Repository: "https://git.example.com/group/other.git"}}
	if err != nil || !reflect.DeepEqual(infos, want) {
		t.Fatalf("metadata = %+v %v", infos, err)
	}
	if text := fmt.Sprintf("%v %+v %#v", got, got, got); strings.Contains(text, gitCredentialToken) || strings.Contains(text, "brood-bot") {
		t.Fatalf("access formatting disclosed the credential: %s", text)
	}
	// Delivered project secrets are a separate store: no revision, file or var appears.
	meta, err := s.GetProjectSecretMetadata(ctx, "p")
	if err != nil || !reflect.DeepEqual(meta, projectsecret.Metadata{Files: []projectsecret.FileInfo{}, Vars: []string{}}) {
		t.Fatalf("Git credential reached delivered secret metadata: %+v %v", meta, err)
	}
	bundle, err := s.ReadProjectSecretBundle(ctx, "p")
	if err != nil || bundle.Revision != 0 || len(bundle.Files) != 0 || len(bundle.Vars) != 0 {
		t.Fatalf("Git credential reached the delivered bundle: %v", err)
	}
	// Other projects see neither the credential nor the reference.
	if err := s.SetProjectRepository(ctx, "q", gitCredentialRepository); err != nil {
		t.Fatal(err)
	}
	if err := s.SetProjectGitCredentialRef(ctx, "q", "gitlab"); err != nil {
		t.Fatal(err)
	}
	if got := access("q"); got.HasCredential() {
		t.Fatal("credential of another project resolved")
	}
	if infos, err := s.ProjectGitCredentials(ctx, "q"); err != nil || len(infos) != 0 {
		t.Fatalf("other project metadata: %+v %v", infos, err)
	}

	// Rotation replaces the value in place.
	if err := s.PutProjectGitCredential(ctx, "p", testGitCredential("gitlab", gitCredentialRepository, "synthetic-rotated-token")); err != nil {
		t.Fatal(err)
	}
	if got := access("p"); got.Credential.Token != "synthetic-rotated-token" {
		t.Fatal("rotation did not replace the token")
	}
	// A repository change unbinds: the token is never resolved for another server.
	if err := s.SetProjectRepository(ctx, "p", "https://git.example.com/group/other.git"); err != nil {
		t.Fatal(err)
	}
	if got := access("p"); got.HasCredential() {
		t.Fatal("credential resolved for a different repository")
	}
	if err := s.SetProjectRepository(ctx, "p", gitCredentialRepository); err != nil {
		t.Fatal(err)
	}
	// Rebinding the name to another repository also unbinds it.
	if err := s.PutProjectGitCredential(ctx, "p", testGitCredential("gitlab", "https://git.example.com/group/other.git", "synthetic-rotated-token")); err != nil {
		t.Fatal(err)
	}
	if got := access("p"); got.HasCredential() {
		t.Fatal("credential resolved after rebinding to another repository")
	}
	if err := s.PutProjectGitCredential(ctx, "p", testGitCredential("gitlab", gitCredentialRepository, "synthetic-rotated-token")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(ctx, path); err != nil {
		t.Fatal(err)
	}
	if got := access("p"); got.Credential.Token != "synthetic-rotated-token" {
		t.Fatal("credential not durable")
	}
	// Removal revokes; absent names are harmless; the reference may dangle.
	if err := s.RemoveProjectGitCredentials(ctx, "p", []string{"gitlab", "absent"}); err != nil {
		t.Fatal(err)
	}
	if got := access("p"); got.HasCredential() || got.Repository != gitCredentialRepository {
		t.Fatal("removed credential still resolves")
	}
	if ref, err := s.ProjectGitCredentialRef(ctx, "p"); err != nil || ref != "gitlab" {
		t.Fatalf("reference after removal: %q %v", ref, err)
	}
	if err := s.SetProjectGitCredentialRef(ctx, "p", ""); err != nil {
		t.Fatal(err)
	}
	if ref, err := s.ProjectGitCredentialRef(ctx, "p"); err != nil || ref != "" {
		t.Fatalf("cleared reference: %q %v", ref, err)
	}
	if err := s.DeleteProject(ctx, "p"); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM project_git_credentials WHERE project_id='p') + (SELECT count(*) FROM project_git_credential_refs WHERE project_id='p')`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("project deletion left Git credential rows: %d %v", rows, err)
	}
}

// SPEC: Invalid input and missing projects are refused before any write and
// without echoing values; the per-project count is bounded, but rotating an
// existing name at the limit is allowed.
func TestProjectGitCredentialRefusals(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedProject(t, s, "p", "p")
	for _, c := range []gitcredential.Credential{
		testGitCredential("Bad", gitCredentialRepository, gitCredentialToken),
		testGitCredential("gitlab", "https://brood-bot:"+gitCredentialToken+"@git.example.com/group/repo.git", gitCredentialToken),
		testGitCredential("gitlab", "https://Git.Example.com/group/repo.git", gitCredentialToken),
		testGitCredential("gitlab", gitCredentialRepository, gitCredentialToken+" "),
		{Name: "gitlab", Repository: gitCredentialRepository, Username: "brood:" + gitCredentialToken, Token: gitCredentialToken},
	} {
		err := s.PutProjectGitCredential(ctx, "p", c)
		if err == nil || strings.Contains(err.Error(), gitCredentialToken) {
			t.Fatalf("invalid credential: %v", err)
		}
	}
	if err := s.PutProjectGitCredential(ctx, "missing", testGitCredential("gitlab", gitCredentialRepository, gitCredentialToken)); !isNotFound(err) {
		t.Fatalf("missing project: %v", err)
	}
	if err := s.SetProjectGitCredentialRef(ctx, "missing", "gitlab"); !isNotFound(err) {
		t.Fatalf("missing project reference: %v", err)
	}
	if err := s.SetProjectGitCredentialRef(ctx, "p", "../gitlab"); err == nil {
		t.Fatal("invalid reference accepted")
	}
	if err := s.RemoveProjectGitCredentials(ctx, "p", []string{"Bad"}); err == nil {
		t.Fatal("invalid removal name accepted")
	}
	if _, err := s.ProjectGitCredentials(ctx, "missing"); !isNotFound(err) {
		t.Fatalf("missing project metadata: %v", err)
	}
	if infos, err := s.ProjectGitCredentials(ctx, "p"); err != nil || len(infos) != 0 {
		t.Fatalf("refused input was written: %+v %v", infos, err)
	}
	for i := range gitcredential.MaxPerProject {
		if err := s.PutProjectGitCredential(ctx, "p", testGitCredential(fmt.Sprintf("c%d", i), gitCredentialRepository, gitCredentialToken)); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PutProjectGitCredential(ctx, "p", testGitCredential("overflow", gitCredentialRepository, gitCredentialToken)); err == nil {
		t.Fatal("credential count limit not enforced")
	}
	if err := s.PutProjectGitCredential(ctx, "p", testGitCredential("c0", gitCredentialRepository, "synthetic-rotated-token")); err != nil {
		t.Fatalf("rotation at the limit: %v", err)
	}
}

// SPEC: A database of the current version whose Git credential tables were
// dropped or weakened is refused on open without mutation.
func TestProjectGitCredentialSchemaDamageRefused(t *testing.T) {
	for _, change := range []string{
		`DROP TABLE project_git_credential_refs`,
		`DROP TABLE project_git_credentials; CREATE TABLE project_git_credentials (project_id TEXT, name TEXT, repository TEXT, username TEXT, token TEXT, PRIMARY KEY (project_id, name))`,
	} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "db.sqlite3")
			s, err := Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.Exec(change); err != nil {
				t.Fatal(err)
			}
			before := schemaSnapshot(t, s.db)
			s.Close()
			if st, err := Open(ctx, path); err == nil {
				st.Close()
				t.Fatal("damaged Git credential schema accepted")
			} else if !strings.Contains(err.Error(), "incompatible database schema") {
				t.Fatal(err)
			}
			if err := assertDatabaseUnchanged(t, path, schemaVersion, before); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// SPEC: Project sync stores the declared reference idempotently: an unchanged
// declaration writes nothing and a member may repeat it, absent keeps the
// reference, "" clears it, a change requires an administrator. Credentials
// themselves are never touched by sync.
func TestProjectSyncGitCredentialReference(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	for _, user := range []User{{ID: "admin", Username: "admin", PasswordHash: "fixture", IsAdmin: true}, {ID: "member", Username: "member", PasswordHash: "fixture"}} {
		if err := s.CreateUser(ctx, user); err != nil {
			t.Fatal(err)
		}
	}
	allow := func(context.Context) bool { return true }
	reference := func(name string) *string { return &name }
	sync := func(user, expected string, declaration ProjectDeclaration) (ProjectSyncResult, error) {
		t.Helper()
		return s.SyncProject(ctx, user, expected, declaration, allow)
	}
	declaration := ProjectDeclaration{Name: "project", Repository: gitCredentialRepository, GitCredential: reference("gitlab")}
	created, err := sync("admin", "", declaration)
	if err != nil || !created.Created || created.GitCredential != "gitlab" {
		t.Fatalf("create with reference: %+v %v", created, err)
	}
	if ref, err := s.ProjectGitCredentialRef(ctx, created.ID); err != nil || ref != "gitlab" {
		t.Fatalf("stored reference: %q %v", ref, err)
	}
	if err := s.AddMember(ctx, created.ID, "member"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutProjectGitCredential(ctx, created.ID, testGitCredential("gitlab", gitCredentialRepository, gitCredentialToken)); err != nil {
		t.Fatal(err)
	}
	for _, user := range []string{"admin", "member"} {
		result, err := sync(user, created.ID, declaration)
		if err != nil || result.Changed || result.GitCredential != "gitlab" {
			t.Fatalf("%s repeat: %+v %v", user, result, err)
		}
	}
	absent := ProjectDeclaration{Name: "project", Repository: gitCredentialRepository}
	if result, err := sync("member", created.ID, absent); err != nil || result.Changed || result.GitCredential != "gitlab" {
		t.Fatalf("absent reference: %+v %v", result, err)
	}
	other := ProjectDeclaration{Name: "project", Repository: gitCredentialRepository, GitCredential: reference("other")}
	if _, err := sync("member", created.ID, other); !errors.Is(err, ErrProjectSyncDenied) {
		t.Fatalf("member changed the reference: %v", err)
	}
	if result, err := sync("admin", created.ID, other); err != nil || !result.Changed || result.GitCredential != "other" {
		t.Fatalf("admin change: %+v %v", result, err)
	}
	cleared := ProjectDeclaration{Name: "project", Repository: gitCredentialRepository, GitCredential: reference("")}
	if result, err := sync("admin", created.ID, cleared); err != nil || !result.Changed || result.GitCredential != "" {
		t.Fatalf("clear: %+v %v", result, err)
	}
	if ref, err := s.ProjectGitCredentialRef(ctx, created.ID); err != nil || ref != "" {
		t.Fatalf("cleared reference: %q %v", ref, err)
	}
	for _, invalid := range []ProjectDeclaration{
		{Name: "project", Repository: gitCredentialRepository, GitCredential: reference("Bad")},
		{Name: "project", Repository: "", GitCredential: reference("gitlab")},
	} {
		if _, err := sync("admin", created.ID, invalid); err == nil || errors.Is(err, ErrProjectSyncDenied) {
			t.Fatalf("invalid declaration: %v", err)
		}
	}
	if infos, err := s.ProjectGitCredentials(ctx, created.ID); err != nil || len(infos) != 1 || infos[0].Name != "gitlab" {
		t.Fatalf("sync touched credentials: %+v %v", infos, err)
	}
}

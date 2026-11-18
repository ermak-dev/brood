package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"brood.dev/go/brood/auth"
)

// FEATURE: Owner policy permission проверяется после чтения HTTP body.
// SPEC: Отзыв ownership/membership/account/session не оставляет окно записи.
func TestProjectOwnerPolicyRechecksAuthorityAfterBodyRead(t *testing.T) {
	for _, change := range []string{"ownership", "membership", "disabled", "session"} {
		t.Run(change, func(t *testing.T) {
			ts, _, alice, _, project := gitRepositorySetup(t)
			ctx := context.Background()
			root, err := ts.srv.store.GetUserByUsername(ctx, "root")
			if err != nil {
				t.Fatal(err)
			}
			user, err := ts.srv.store.GetUserByUsername(ctx, "alice")
			if err != nil {
				t.Fatal(err)
			}
			_, err = ts.srv.store.SetProjectOwner(ctx, root.ID, project, 0, "alice", func(context.Context) bool { return true })
			if err != nil {
				t.Fatal(err)
			}
			read := false
			reader := &projectSecretAuthorityReader{Reader: bytes.NewReader([]byte(`{"revision":0,"rules":[]}`)), beforeRead: func() {
				read = true
				var err error
				switch change {
				case "ownership":
					_, err = ts.srv.store.SetProjectOwner(ctx, root.ID, project, 1, "", func(context.Context) bool { return true })
				case "membership":
					err = ts.srv.store.RemoveMember(ctx, project, user.ID)
				case "disabled":
					err = ts.srv.store.SetUserDisabled(ctx, user.ID, true)
				case "session":
					_, err = auth.NewSessions(ts.srv.store, 0).RevokeAllOfUser(ctx, user.ID)
				}
				if err != nil {
					t.Fatal(err)
				}
			}}
			req := httptest.NewRequest("PUT", ts.http.URL+"/api/projects/"+project+"/workflow-policy", reader)
			req.Header.Set("Origin", ts.http.URL)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set(auth.HeaderCSRF, ts.csrf(alice))
			for _, cookie := range alice.Jar.Cookies(req.URL) {
				req.AddCookie(cookie)
			}
			recorder := httptest.NewRecorder()
			ts.srv.Handler().ServeHTTP(recorder, req)
			if !read || recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "authorization changed") {
				t.Fatalf("late owner authority %s: read=%v status=%d body=%s", change, read, recorder.Code, recorder.Body.String())
			}
			policy, err := ts.srv.store.ProjectWorkflowPolicy(ctx, project)
			if err != nil || policy.Revision != 0 {
				t.Fatal("revoked owner changed project policy")
			}
		})
	}
}

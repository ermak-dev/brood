//go:build linux

package guest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// BUG: Git's failed-clone cleanup removes concurrent live workspace data (#153).
// HYPOTHESIS: Cloning directly into the live directory delegates destructive
// cleanup to Git even when the wrapper never removes a workspace file.
// SOLUTION: Clone in private staging, then publish with no-replace renames.
// RESULT: The real Git HTTP 503 reproduction keeps concurrent user work.
func TestClonePublishRefusesConcurrentWorkspaceWork(t *testing.T) {
	for _, name := range []string{"README", "unrelated"} {
		t.Run(name, func(t *testing.T) {
			project, home := privateTemp(t), privateTemp(t)
			git := func(_ context.Context, _ []string, args ...string) error {
				if args[len(args)-4] == "clone" {
					destination := args[len(args)-1]
					if err := os.Mkdir(filepath.Join(destination, ".git"), 0700); err != nil {
						return err
					}
					return os.WriteFile(filepath.Join(destination, "README"), []byte("checkout\n"), 0600)
				}
				return os.WriteFile(filepath.Join(project, name), []byte("user\n"), 0600)
			}
			err := cloneWorkspace(context.Background(), cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Timeout: time.Second, Git: git})
			if err == nil {
				t.Fatal("concurrent workspace change not refused")
			}
			data, err := os.ReadFile(filepath.Join(project, name))
			if err != nil || string(data) != "user\n" {
				t.Fatalf("concurrent work changed: %q %v", data, err)
			}
			if _, err := os.Stat(filepath.Join(project, ".git")); !os.IsNotExist(err) {
				t.Fatalf("checkout published over user work: %v", err)
			}
		})
	}
}

func TestRealGitFailurePreservesConcurrentWorkspaceWork(t *testing.T) {
	project, home := privateTemp(t), privateTemp(t)
	wrote := make(chan error, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := os.WriteFile(filepath.Join(project, "user-work"), []byte("keep\n"), 0600)
		select {
		case wrote <- err:
		default:
		}
		http.Error(w, "upstream unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	git := func(ctx context.Context, env []string, args ...string) error {
		args[len(args)-2] = server.URL + "/origin.git"
		return runCloneGit(ctx, append(env, "GIT_SSL_NO_VERIFY=1"), args...)
	}
	err := cloneWorkspace(context.Background(), cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Timeout: 10 * time.Second, Git: git})
	if err == nil {
		t.Fatal("upstream failure not reported")
	}
	select {
	case err := <-wrote:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("Git never reached upstream")
	}
	data, err := os.ReadFile(filepath.Join(project, "user-work"))
	if err != nil || string(data) != "keep\n" {
		t.Fatalf("Git failure deleted concurrent user work: %q %v", data, err)
	}
}

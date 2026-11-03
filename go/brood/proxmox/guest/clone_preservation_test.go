//go:build linux

package guest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// BUG: A pending marker or failed Git process is not proof that all files are clone residue.
// SPEC: User work created between attempts or during Git execution is never automatically removed.
func TestClonePreservesWorkAfterInterruptedAttempt(t *testing.T) {
	home, project := privateTemp(t), privateTemp(t)
	state, err := secretManifestDirectory(home)
	if err != nil {
		t.Fatal(err)
	}
	defer os.NewFile(uintptr(state), "state").Close()
	if err := replaceSecretFile(state, cloneMarker, []byte(cloneTestOrigin+"\n"), 0600, os.Geteuid(), os.Getegid(), false); err != nil {
		t.Fatal(err)
	}
	// Pending относится к прерванной публикации в live, не к удаляемому staging.
	if err := os.WriteFile(filepath.Join(project, "user-work"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	spec := cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Mode: "gateway", Gateway: workspaceGatewayURL, Timeout: time.Second, Git: func(context.Context, []string, ...string) error { t.Fatal("clone ran over user work"); return nil }}
	if err := cloneWorkspace(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "workspace preserved") {
		t.Fatalf("interrupted live workspace did not require explicit recovery: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(project, "user-work")); err != nil || string(data) != "keep" {
		t.Fatal("user work removed")
	}
}

func TestCloneFailurePreservesConcurrentWork(t *testing.T) {
	home, project := privateTemp(t), privateTemp(t)
	var stage string
	spec := cloneSpec{Project: project, Home: home, Origin: cloneTestOrigin, Mode: "gateway", Gateway: workspaceGatewayURL, Timeout: time.Second, Git: func(_ context.Context, _ []string, args ...string) error {
		stage = cloneTestDestination(t, args, project, home)
		if err := os.MkdirAll(filepath.Join(stage, ".git", "objects"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(project, "user-work"), []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
		return errors.New("git stopped")
	}}
	if cloneWorkspace(context.Background(), spec) == nil {
		t.Fatal("failed clone reported success")
	}
	if data, err := os.ReadFile(filepath.Join(project, "user-work")); err != nil || string(data) != "keep" {
		t.Fatal("concurrent user work removed")
	}
	if names := entries(t, project); !slices.Equal(names, []string{"user-work"}) {
		t.Fatalf("failed stage changed live workspace: %v", names)
	}
	if _, err := os.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed stage was not removed: %v", err)
	}
}

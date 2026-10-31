//go:build linux

package guest

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"testing"
)

// Command failures must retain a cause instead of silently disabling enforcement.
func TestNFTRetainsFailureCause(t *testing.T) {
	reason := errors.New("CAP_NET_ADMIN unavailable")
	err := applyRules(context.Background(), nftRules(), func(context.Context, string, []string, string) error { return reason })
	if !errors.Is(err, reason) {
		t.Fatalf("nft cause lost: %v", err)
	}
	if err = applyRules(context.Background(), nftRules(), nil); err == nil {
		t.Fatal("missing nft command accepted")
	}
}

func TestActorCanFindPinnedGoAndKeepsBoundedBuilds(t *testing.T) {
	for _, want := range []string{"PATH=/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin", "GOTOOLCHAIN=local", "GOMAXPROCS=4", "GOFLAGS=-p=2", "DISABLE_AUTOUPDATER=1"} {
		if !slices.Contains(actorEnv(), want) {
			t.Errorf("missing fixed guest environment: %s", want)
		}
	}
}

// The privilege lock must survive exec, not just a Go-level credential check.
func TestPrivilegeLockSurvivesExec(t *testing.T) {
	if os.Getenv("GUEST_EXEC_TEST") == "1" {
		if err := lockPrivileges(); err != nil {
			os.Exit(2)
		}
		cmd := exec.Command("/bin/cat", "/proc/self/status")
		cmd.Env = actorEnv()
		status, err := cmd.Output()
		if err != nil {
			os.Exit(3)
		}
		for _, want := range []string{"NoNewPrivs:\t1", "CapEff:\t0000000000000000", "CapAmb:\t0000000000000000"} {
			if !bytes.Contains(status, []byte(want)) {
				os.Exit(4)
			}
		}
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPrivilegeLockSurvivesExec$")
	cmd.Env = append(os.Environ(), "GUEST_EXEC_TEST=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

//go:build linux

package guest

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"testing"
)

func TestDrainDetachedActor(t *testing.T) {
	calls := 0
	if err := drainActorProcesses(context.Background(), func() (bool, error) { calls++; return calls < 3, nil }); err != nil || calls != 3 {
		t.Fatalf("%d %v", calls, err)
	}
	sentinel := errors.New("cannot inspect UID")
	if err := drainActorProcesses(context.Background(), func() (bool, error) { return false, sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := drainActorProcesses(ctx, func() (bool, error) { t.Error("swept after cancellation"); return true, nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		status string
		want   bool
		valid  bool
	}{{"Name:\ttmux\nState:\tS (sleeping)\nUid:\t1000\t1000\t1000\t1000\n", true, true}, {"Name:\troot\nState:\tS (sleeping)\nUid:\t0\t0\t0\t0\n", false, true}, {"State:\tZ (zombie)\nUid:\t1000\t1000\t1000\t1000\n", false, true}, {"State:\tS\nUid:\t0\t1000\t0\t0\n", true, true}, {"garbage", false, false}} {
		got, err := actorStatus([]byte(tc.status))
		if got != tc.want || (err == nil) != tc.valid {
			t.Fatalf("%q: %v %v", tc.status, got, err)
		}
	}
}

type closeFailure struct{ Process }

func (p closeFailure) Close() error { return errors.New("descendant still alive") }

type signalProcess struct {
	*fakeProcess
	err error
}

func (p signalProcess) Wait() error { <-p.done; return p.err }
func TestCloseReportsTerminationFailure(t *testing.T) {
	var events []string
	c := testConfig(&events)
	var child *fakeProcess
	c.Dependencies.Launch = func(context.Context, ActorSpec) (Process, error) {
		child = &fakeProcess{done: make(chan struct{}), events: &events}
		return closeFailure{child}, nil
	}
	h, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err == nil {
		t.Fatal("termination failure hidden")
	}
	_ = child.Close()
}
func TestIntentionalKillIsNotWaitFailure(t *testing.T) {
	var events []string
	c := testConfig(&events)
	cmd := exec.Command("/bin/sh", "-c", "kill -KILL $$")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected test subprocess signal")
	}
	c.Dependencies.Launch = func(context.Context, ActorSpec) (Process, error) {
		return signalProcess{&fakeProcess{done: make(chan struct{}), events: &events}, err}, nil
	}
	h, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Close(); err != nil {
		t.Fatalf("intentional actor termination: %v", err)
	}
}
func TestMissingDependencyAndCanceledContext(t *testing.T) {
	for _, field := range []string{"EUID", "Workspace", "Launch"} {
		var events []string
		c := testConfig(&events)
		reflect.ValueOf(c.Dependencies).Elem().FieldByName(field).SetZero()
		if h, err := Start(context.Background(), c); h != nil || err == nil || len(events) != 0 {
			t.Fatalf("%s: %v %v", field, events, err)
		}
	}
	var events []string
	c := testConfig(&events)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if h, err := Start(ctx, c); h != nil || !errors.Is(err, context.Canceled) || len(events) != 0 {
		t.Fatalf("%v %v", events, err)
	}
}
func TestWorkspaceUnsafeModesAndFiles(t *testing.T) {
	root := privateTemp(t)
	if err := os.Chmod(root, 0777); err != nil {
		t.Fatal(err)
	}
	if err := prepareWorkspace(root, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("writable workspace accepted")
	}
	root = privateTemp(t)
	if err := os.WriteFile(root+"/project", nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := prepareWorkspace(root, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("project file accepted")
	}
}

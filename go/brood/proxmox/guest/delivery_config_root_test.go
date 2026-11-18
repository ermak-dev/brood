//go:build linux

package guest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"brood.dev/go/brood/proxmox/guest/deliverylog"
)

func TestSupervisorImageConfigurationRootIntegration(t *testing.T) {
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		t.Skip("requires an isolated root guest with trusted private TMPDIR")
	}
	trusted, err := openDeliveryRoot(os.TempDir())
	if err != nil {
		t.Skip("requires a trusted private TMPDIR, not a shared temporary directory")
	}
	trusted.Close()
	path := t.TempDir()
	for _, dir := range []string{filepath.Dir(path), path} {
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	configDir, stateDir := filepath.Join(path, "config"), filepath.Join(path, "state")
	if got, err := ReadSupervisorDeliveryDirectory(ctx, configDir); err != nil || got != "" {
		t.Fatalf("old image without configuration stopped: %q, %v", got, err)
	}
	for _, dir := range []string{configDir, stateDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := ReadSupervisorDeliveryDirectory(ctx, configDir); err != nil || got != "" {
		t.Fatalf("missing configuration file stopped legacy runtime: %q, %v", got, err)
	}
	configPath := filepath.Join(configDir, "delivery-state-dir")
	if err := os.WriteFile(configPath, []byte(stateDir+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadSupervisorDeliveryDirectory(ctx, configDir)
	if err != nil || got != stateDir {
		t.Fatalf("trusted image configuration refused: %q, %v", got, err)
	}
	scope := deliverylog.Scope{ActorUID: "image-actor", Incarnation: 3}
	journal, err := OpenSupervisorDeliveryJournal(ctx, got, scope)
	if err != nil {
		t.Fatal(err)
	}
	identity := journal.Identifier()
	admission, err := journal.Begin(ctx, "image-delivery", []byte("original payload"))
	if err != nil {
		journal.Close()
		t.Fatal(err)
	}
	if err := journal.Finish(ctx, admission.Ticket, deliverylog.Delivered); err != nil {
		journal.Close()
		t.Fatal(err)
	}
	journal.Close()
	got, err = ReadSupervisorDeliveryDirectory(ctx, configDir)
	if err != nil {
		t.Fatal(err)
	}
	journal, err = OpenSupervisorDeliveryJournal(ctx, got, scope)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if journal.Identifier() != identity {
		t.Fatal("runtime reopen replaced retained journal identity")
	}
	if state, err := journal.Lookup(ctx, "image-delivery", []byte("original payload")); err != nil || state != deliverylog.Duplicate {
		t.Fatalf("runtime reopen lost retained receipt: %v, %v", state, err)
	}
	if err := os.Chmod(configPath, 0666); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSupervisorDeliveryDirectory(ctx, configDir); !errors.Is(err, errDeliveryConfiguration) {
		t.Fatalf("unsafe file permissions silently downgraded to legacy: %v", err)
	}
	if err := os.Chmod(configPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(configDir, filepath.Join(path, "linked-config")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadSupervisorDeliveryDirectory(ctx, filepath.Join(path, "linked-config")); !errors.Is(err, errDeliveryConfiguration) {
		t.Fatalf("image configuration followed a symbolic link: %v", err)
	}
}

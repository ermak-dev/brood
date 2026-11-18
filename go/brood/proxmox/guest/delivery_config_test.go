//go:build linux

package guest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func deliveryConfigFixture(t *testing.T) (*os.File, string) {
	t.Helper()
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dir.Close() })
	return dir, filepath.Join(path, "delivery-state-dir")
}

func TestDeliveryImageConfigurationReadsExplicitDirectoryFromPinnedFile(t *testing.T) {
	dir, path := deliveryConfigFixture(t)
	if err := os.WriteFile(path, []byte("/var/lib/brood/delivery\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := readDeliveryDirectory(context.Background(), dir, uint32(os.Geteuid()))
	if err != nil || got != "/var/lib/brood/delivery" {
		t.Fatalf("image configuration lost explicit directory: %q, %v", got, err)
	}
	if err := os.Rename(filepath.Dir(path), filepath.Dir(path)+"-retained"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		os.RemoveAll(filepath.Dir(path))
		os.Rename(filepath.Dir(path)+"-retained", filepath.Dir(path))
	})
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("/different/delivery\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = readDeliveryDirectory(context.Background(), dir, uint32(os.Geteuid()))
	if err != nil || got != "/var/lib/brood/delivery" {
		t.Fatalf("configuration followed replaced pathname: %q, %v", got, err)
	}
}

func TestDeliveryImageConfigurationMissingFileRemainsLegacy(t *testing.T) {
	dir, _ := deliveryConfigFixture(t)
	got, err := readDeliveryDirectory(context.Background(), dir, uint32(os.Geteuid()))
	if err != nil || got != "" {
		t.Fatalf("missing image configuration is not legacy: %q, %v", got, err)
	}
}

func TestDeliveryImageConfigurationRejectsInvalidContents(t *testing.T) {
	for i, value := range []string{"", "\n", "relative\n", "/private/../delivery\n", "/private/delivery/\n", "/\n", "/private/delivery", "/one\n/two\n", "/one\r\n", "/one\x00\n", "/one\xff\n", strings.Repeat("/a", 2048) + "\n"} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			dir, path := deliveryConfigFixture(t)
			if err := os.WriteFile(path, []byte(value), 0600); err != nil {
				t.Fatal(err)
			}
			got, err := readDeliveryDirectory(context.Background(), dir, uint32(os.Geteuid()))
			if err == nil || got != "" || strings.Contains(err.Error(), value) && value != "" {
				t.Fatalf("invalid configuration accepted or reflected: %q, %v", got, err)
			}
		})
	}
}

func TestDeliveryImageConfigurationRejectsUntrustedFileWithoutRepair(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "directory", "fifo", "permissions", "owner"} {
		t.Run(kind, func(t *testing.T) {
			dir, path := deliveryConfigFixture(t)
			owner := uint32(os.Geteuid())
			if kind == "directory" {
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			} else if kind == "fifo" {
				if err := unix.Mkfifo(path, 0600); err != nil {
					t.Fatal(err)
				}
			} else if kind == "symlink" {
				target := filepath.Join(t.TempDir(), "target")
				if err := os.WriteFile(target, []byte("/private/delivery\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.WriteFile(path, []byte("/private/delivery\n"), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "hardlink" {
					if err := os.Link(path, path+"-copy"); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "permissions" {
					if err := os.Chmod(path, 0644); err != nil {
						t.Fatal(err)
					}
				}
				if kind == "owner" {
					owner++
				}
			}
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			got, err := readDeliveryDirectory(context.Background(), dir, owner)
			if err == nil || got != "" {
				t.Fatalf("unsafe file accepted: %q, %v", got, err)
			}
			after, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, after) || before.Mode() != after.Mode() {
				t.Fatal("configuration reader repaired or replaced an unsafe file")
			}
		})
	}
}

func TestDeliveryImageConfigurationCancellationAndRootBoundary(t *testing.T) {
	dir, _ := deliveryConfigFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := readDeliveryDirectory(ctx, dir, uint32(os.Geteuid())); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read was not canceled: %v", err)
	}
	if os.Getuid() != 0 || os.Geteuid() != 0 {
		if _, err := ReadSupervisorDeliveryDirectory(context.Background(), dir.Name()); err == nil {
			t.Fatal("non-root process read trusted supervisor configuration")
		}
	}
	if _, err := ReadSupervisorDeliveryDirectory(context.Background(), "relative"); err == nil {
		t.Fatal("relative configuration directory accepted")
	}
}

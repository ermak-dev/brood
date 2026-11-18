package deliverylog

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestFailedSettlementSyncCannotProduceReceiptWithoutDurability(t *testing.T) {
	for _, operation := range []string{"begin", "lookup", "finish", "other-handle"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			journal := openTest(t, dir, 4)
			reservation, err := journal.Begin(ctx, "step:sync", []byte("message"))
			if err != nil || reservation.State != New {
				t.Fatalf("reservation: %+v %v", reservation, err)
			}
			reader := journal
			if operation == "other-handle" {
				reader = openTest(t, dir, 4)
			}
			path := filepath.Join(dir, journalName)
			fault := func() error {
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				if bytes.Contains(data, []byte(`"state":"delivered"`)) {
					return unix.EIO
				}
				return journal.file.Sync()
			}
			journal.syncFileFn = fault
			reader.syncFileFn = fault
			if err := journal.Finish(ctx, reservation.Ticket, Delivered); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("failed settlement sync: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(data, []byte(`"state":"delivered"`)) {
				t.Fatalf("did not exercise complete write followed by failed sync: %v", err)
			}
			check := func() error {
				switch operation {
				case "begin":
					result, err := reader.Begin(ctx, "step:sync", []byte("message"))
					if err == nil && result.State != Duplicate {
						t.Fatalf("unexpected admission after recovery: %+v", result)
					}
					return err
				case "lookup", "other-handle":
					state, err := reader.Lookup(ctx, "step:sync", []byte("message"))
					if err == nil && state != Duplicate {
						t.Fatalf("unexpected receipt after recovery: %s", state)
					}
					return err
				default:
					return reader.Finish(ctx, reservation.Ticket, Delivered)
				}
			}
			if err := check(); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("acknowledged unsynchronized settlement: %v", err)
			}
			reader.syncFileFn = nil
			if err := check(); err != nil {
				t.Fatalf("receipt was not recoverable after a successful durability barrier: %v", err)
			}
		})
	}
}

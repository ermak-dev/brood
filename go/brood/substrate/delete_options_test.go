package substrate

import (
	"context"
	"testing"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

func TestAtespaceDeleteSendsUIDPrecondition(t *testing.T) {
	fake := newFake()
	fake.atespaces["test"] = &ateapi.Atespace{Metadata: meta("test", "")}
	client, _ := startFake(t, fake)
	if err := client.DeleteAtespace(context.Background(), "test", "saved-uid"); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.deleteUID != "saved-uid" {
		t.Fatalf("delete lost UID precondition: %q", fake.deleteUID)
	}
}

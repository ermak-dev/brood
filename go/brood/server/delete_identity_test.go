package server

import (
	"context"
	"errors"
	"testing"

	"brood.dev/go/brood/app"
	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

func TestProvisionerCannotDeleteNamespaceWithoutRecordedUID(t *testing.T) {
	fixture := newSubstrateServer(t, newFakeAteapi())
	_, err := fixture.srv.ateapi.CreateAtespace(context.Background(), &ateapi.Atespace{Metadata: &ateapi.ResourceMetadata{Name: "foreign"}})
	if err != nil {
		t.Fatal(err)
	}
	provisioner := NewSubstrateProvisioner(fixture.srv.ateapi)
	if err := provisioner.DeleteAtespace("foreign", ""); !errors.Is(err, app.ErrConflict) {
		t.Fatalf("unguarded namespace delete: %v", err)
	}
	if _, err := fixture.srv.ateapi.GetAtespace(context.Background(), "foreign"); err != nil {
		t.Fatalf("foreign namespace was deleted: %v", err)
	}
}

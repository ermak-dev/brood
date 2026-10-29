package substrate

import (
	"context"
	"errors"
	"testing"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// FEATURE: Атомарное удаление только прочитанного native объекта (#78).
// SPEC: UID обязателен; поздняя замена под тем же именем сохраняется,
// native ABORTED остаётся HTTP409, а отсутствие UID не вызывает RPC.
func TestDeleteUIDPreconditions(t *testing.T) {
	const uid = "093a17fb-d3ce-4edf-bac5-42401d2807b1"
	for _, kind := range []string{"actor", "template"} {
		for _, requested := range []string{uid, "77361a3f-2447-4e51-9bd4-0053f75d4551"} {
			t.Run(kind+requested, func(t *testing.T) {
				fake := newFake()
				metadata := &ateapi.ResourceMetadata{Name: "subject", Atespace: "space", Uid: uid}
				fake.actors["space/subject"] = &ateapi.Actor{Metadata: metadata}
				fake.templates["space/subject"] = &ateapi.ActorTemplate{Metadata: metadata}
				client, _ := startFake(t, fake)
				var err error
				if kind == "actor" {
					err = client.DeleteActorUID(context.Background(), "space", "subject", requested)
				} else {
					err = client.DeleteActorTemplateUID(context.Background(), "space", "subject", requested)
				}
				fake.mu.Lock()
				defer fake.mu.Unlock()
				if fake.deleteUID != requested {
					t.Fatalf("RPC uid = %q, want %q", fake.deleteUID, requested)
				}
				remaining := fake.actors["space/subject"] != nil
				if kind == "template" {
					remaining = fake.templates["space/subject"] != nil
				}
				if requested == uid {
					if err != nil || remaining {
						t.Fatalf("matching identity was not deleted: %v", err)
					}
				} else {
					var api *APIError
					if !errors.As(err, &api) || api.Status != 409 || !remaining {
						t.Fatalf("replacement identity was affected: %v, remaining %v", err, remaining)
					}
				}
			})
		}
	}
}

func TestDeleteUIDRequiresIdentityBeforeConnecting(t *testing.T) {
	client := &Client{}
	for _, remove := range []func(context.Context, string, string, string) error{
		client.DeleteActorUID, client.DeleteActorTemplateUID,
	} {
		var api *APIError
		if err := remove(context.Background(), "space", "subject", ""); !errors.As(err, &api) || api.Status != 400 {
			t.Fatalf("empty identity did not fail before RPC: %v", err)
		}
	}
}

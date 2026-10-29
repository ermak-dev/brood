package substrate

import (
	"context"
	"strings"

	ateapi "go.ate.dev/substrate/api/ateapi/v1"
)

// DeleteActorUID deletes only the actor with the supplied native UID.
// An empty UID is refused before making a request.
func (c *Client) DeleteActorUID(ctx context.Context, atespace, name, uid string) error {
	if strings.TrimSpace(uid) == "" {
		return &APIError{Status: 400, Detail: "actor UID is required"}
	}
	_, err := c.stub().DeleteActor(ctx, &ateapi.DeleteActorRequest{
		Actor: ref(name, atespace), Options: &ateapi.DeleteOptions{Uid: uid},
	})
	if err != nil {
		return mapGRPC(err)
	}
	return nil
}

// DeleteActorTemplateUID deletes only the template with the supplied native UID.
// An empty UID is refused before making a request.
func (c *Client) DeleteActorTemplateUID(ctx context.Context, atespace, name, uid string) error {
	if strings.TrimSpace(uid) == "" {
		return &APIError{Status: 400, Detail: "template UID is required"}
	}
	_, err := c.stub().DeleteActorTemplate(ctx, &ateapi.DeleteActorTemplateRequest{
		ActorTemplate: ref(name, atespace), Options: &ateapi.DeleteOptions{Uid: uid},
	})
	if err != nil {
		return mapGRPC(err)
	}
	return nil
}

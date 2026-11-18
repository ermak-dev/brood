package server

import (
	"testing"

	"brood.dev/go/brood/store"
)

func TestTelegramPolicyButtonCarriesOriginalQuestionRevision(t *testing.T) {
	for option, response := range []string{"yes", "no"} {
		request := store.SwarmRequest{ID: "policy:candidate", SwarmID: "swarm", Origin: "policy", Kind: "confirm", State: "open"}
		outcome, text, ok := telegramButtonAnswer(request, option)
		if !ok || outcome != "" || text != response {
			t.Fatalf("policy button did not map to a confirmation: option=%d outcome=%q response=%q ok=%v", option, outcome, text, ok)
		}
		in := telegramRequestAnswer(request, "owner", outcome, text)
		if in.UserID != "owner" || in.SwarmID != "swarm" || in.RequestID != request.ID || in.ExpectedRevision != 1 || in.Response != response || in.Outcome != "" {
			t.Fatalf("policy button lost scope/revision: %+v", in)
		}
		request.State = "answered"
		if retry := telegramRequestAnswer(request, "owner", outcome, text); retry.ExpectedRevision != 1 {
			t.Fatal("old button was rebound to the live answered revision")
		}
	}
	request := store.SwarmRequest{Origin: "actor", Kind: "confirm"}
	if in := telegramRequestAnswer(request, "owner", "", "yes"); in.ExpectedRevision != 0 {
		t.Fatal("legacy actor confirmation acquired policy semantics")
	}
}

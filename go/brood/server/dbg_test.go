package server

import "testing"

func TestSubstrateProvisionCreatesActiveSwarm(t *testing.T) {
	fake := newFakeAteapi()
	ts := newSubstrateServer(t, fake)
	_, _, _, swarmID := actorsSetup(t, ts.testServer)
	sw, err := ts.srv.store.GetSwarm(nil2ctx(), swarmID)
	if err != nil {
		t.Fatal(err)
	}
	if sw.State != "active" || sw.AtespaceUID == nil || *sw.AtespaceUID == "" {
		t.Fatalf("swarm is not provisioned: state=%s uid=%v", sw.State, sw.AtespaceUID)
	}
}

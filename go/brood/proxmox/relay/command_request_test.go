package relay

import "testing"

// FEATURE: Shared command arguments on the authenticated actor RPC.
// SPEC: Canonical operations are bounded and validated; legacy and canonical arguments cannot mix.
func TestCommandRequestEnvelope(t *testing.T) {
	for _, body := range []string{
		`{"tool":"profile","arguments":{}}`,
		`{"tool":"tail","arguments":{"name":"worker","lines":20}}`,
		`{"tool":"send","arguments":{"to":"worker","message":"hello","wait_seconds":1}}`,
	} {
		if _, ok := DecodeSwarmRequest([]byte(body)); !ok {
			t.Errorf("canonical request rejected: %s", body)
		}
	}
	for _, body := range []string{
		`{"tool":"profile","arguments":{"uid":"other"}}`,
		`{"tool":"tail","arguments":{"name":"worker","path":"/etc/passwd"}}`,
		`{"tool":"send","to":"worker","arguments":{"to":"other","message":"hello"}}`,
		`{"tool":"profile","arguments":{},"tool":"agents"}`,
		`{"tool":"unknown","arguments":{}}`,
		`{"tool":"tail","arguments":{"name":"worker","name":"other"}}`,
	} {
		if _, ok := DecodeSwarmRequest([]byte(body)); ok {
			t.Errorf("invalid canonical request accepted: %s", body)
		}
	}
}

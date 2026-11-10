package server

import "brood.dev/go/brood/proxmox/attest"

// workspaceAttestationKey derives the attestation key of an issued incarnation
// from the installation relay secret, as the guest derives it from the
// bootstrap token createSpec gave it. Step delivery (#212) passes it to
// attest.Registry.Consume, which calls it with the issued identity only.
func workspaceAttestationKey(relaySecret string) func(string, int64) []byte {
	return func(uid string, generation int64) []byte {
		return attest.Key(guestToken(relaySecret, uid, generation))
	}
}

package store

import "context"

// CheckEphemeralChildAttestation rechecks a pending attempt immediately before
// opening its one authorized workspace RPC. It never grants fresh effect
// permission: the caller must already hold a fresh durable reservation result.
// Recovered/unknown attempts and owner interventions cannot pass this check.
func (s *Store) CheckEphemeralChildAttestation(ctx context.Context, intentID, attemptID string, authorize SwarmChildAuthorize) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		out, err := s.currentEphemeralAttestation(ctx, intentID, attemptID, authorize)
		if err != nil {
			return err
		}
		if out.State != "pending" {
			return ErrSwarmChildConflict
		}
		lease, err := s.CurrentActorOperation(ctx, out.ActorUID)
		if err != nil {
			return err
		}
		if lease.State != "inflight" {
			return ErrSwarmChildConflict
		}
		return s.checkEphemeralAttestationLease(ctx, out)
	})
}

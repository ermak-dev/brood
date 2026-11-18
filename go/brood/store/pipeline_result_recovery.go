package store

import "context"

// RecoverPipelineResultObservations retains abandoned effects as unconfirmed.
// Call only during exclusive startup. It never resends, releases slots, changes
// other operations or treats actor-reported completion as verified result evidence.
func (s *Store) RecoverPipelineResultObservations(ctx context.Context) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		_, err := s.conn(ctx).ExecContext(ctx, `UPDATE coordination_claims AS c SET payload=json_set(payload,'$.State','unconfirmed') WHERE c.kind='workspace' AND c.slot IS NOT NULL AND EXISTS (
 SELECT 1 FROM pipeline_result_observations o JOIN pipeline_completion_intents i ON i.swarm_id=o.swarm_id AND i.sequence=o.sequence
 WHERE o.state='inflight' AND c.request_id=o.id AND c.slot=i.actor_uid
 AND json_extract(c.payload,'$.OperationID')=o.id AND json_extract(c.payload,'$.UID')=i.actor_uid
 AND json_extract(c.payload,'$.Generation')=i.actor_incarnation AND json_extract(c.payload,'$.Operation')='workspace_result'
 AND json_extract(c.payload,'$.ProjectID')=c.project_id
 )`)
		if err != nil {
			return err
		}
		_, err = s.conn(ctx).ExecContext(ctx, `UPDATE pipeline_result_observations SET state='unconfirmed',detail='The server restarted during result observation; the outcome is unknown.',revision=revision+1 WHERE state='inflight'`)
		return err
	})
}

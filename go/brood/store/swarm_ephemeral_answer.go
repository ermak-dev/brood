package store

import "context"

// CheckEphemeralChildAnswer checks an answered question's exact retained child
// context and current running execution scope in one read-only snapshot. The
// mandatory local callback receives action deliver_ephemeral_answer with Caller
// equal to the executor. Trusted adapters must separately authorize the human
// answer and retain ordinary delivery reservation/journal/effect gates. This
// method never grants a fresh send, rewrites history or completes child work.
func (s *Store) CheckEphemeralChildAnswer(ctx context.Context, swarmID, requestID string, authorize SwarmChildAuthorize) error {
	if authorize == nil {
		return ErrSwarmRequestDenied
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		q, err := s.GetSwarmRequest(ctx, swarmID, requestID)
		if err != nil {
			return err
		}
		if q.Origin != "actor" || q.State != "answered" || q.Response == nil {
			return ErrSwarmRequestDenied
		}
		intent, err := s.GetEphemeralChildIntentForActor(ctx, q.ActorUID, q.ActorIncarnation)
		if err != nil {
			return err
		}
		retained, err := s.GetSwarmRequestPassContext(ctx, swarmID, requestID)
		if err != nil {
			return err
		}
		if retained.Pass != intent.Parent.Pass || retained.ObservedSequence != intent.Parent.Sequence || retained.SwarmID != intent.Parent.Principal.SwarmID {
			return ErrSwarmRequestDenied
		}
		join, err := s.loadChildJoin(ctx, intent.Parent)
		if err != nil {
			return err
		}
		for _, child := range join.Children {
			if child.RequestID == intent.RequestID && child.Executor.ActorUID == q.ActorUID && child.Executor.Incarnation == q.ActorIncarnation {
				_, err := s.ephemeralQuestionParent(ctx, child.ID, ActorRequestInput{SwarmID: q.SwarmID, ActorUID: q.ActorUID, ActorIncarnation: q.ActorIncarnation}, "deliver_ephemeral_answer", authorize)
				return err
			}
		}
		return ErrSwarmRequestDenied
	})
}

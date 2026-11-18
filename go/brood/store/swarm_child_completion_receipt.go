package store

import "context"

// AcceptSwarmChildJoin records a server-computed parent completion and receipt
// only after every child obligation settles. It never advances the parent graph.
func (s *Store) AcceptSwarmChildJoin(ctx context.Context, parent SwarmChildParent, nonce string, authorize SwarmChildAuthorize) (SwarmChildJoin, error) {
	var result SwarmChildJoin
	err := s.WithTx(ctx, func(ctx context.Context) error {
		joined, err := s.GetSwarmChildJoin(ctx, parent, authorize)
		if err != nil {
			return err
		}
		result = joined
		if joined.Evaluation.Open != 0 || joined.Interrupted || joined.State == "aborted" {
			return nil
		}
		outcome := "fail"
		if joined.Evaluation.State == "ready_success" {
			outcome = "success"
		}
		in := ActorCompletion{SwarmID: parent.Principal.SwarmID, Sequence: parent.Sequence, RequestID: parent.RequestID, ActorUID: parent.Principal.ActorUID, Incarnation: parent.Principal.Incarnation, Outcome: outcome, Report: ChildJoinReport(joined)}
		receipt, err := s.AcceptPipelineCompletion(context.WithValue(ctx, childJoinCompletionKey{}, true), in, nonce, func(ctx context.Context) bool {
			return s.authorizeChild(ctx, SwarmChildAuthorization{Action: "read", Caller: parent.Principal, Parent: parent}, authorize) == nil
		})
		if err != nil {
			return err
		}
		result.CompletionReceipt = &receipt
		return nil
	})
	if err != nil {
		return SwarmChildJoin{}, err
	}
	return result, nil
}

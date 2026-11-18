package store

import (
	"context"
	"errors"
)

// ErrNoPolicyHuman means this result does not target a human graph node.
var ErrNoPolicyHuman = errors.New("policy result does not target a human node")

// ResolvePipelinePolicyHuman applies a verified actor result to its exact human
// successor only after policy obligations are satisfied. The policy answer and
// the resulting graph question remain separate records and separate decisions.
// Callers must establish current external actor scope before entering the transaction.
func (s *Store) ResolvePipelinePolicyHuman(ctx context.Context, swarmID string, sequence int64) (PipelinePolicyTransitionResolution, error) {
	return s.resolvePipelinePolicyTransition(ctx, swarmID, sequence, "human", ErrNoPolicyHuman)
}

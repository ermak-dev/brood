package store

import (
	"context"
	"errors"
)

// AnswerPipelineVisit отвечает на узловой запрос ждущего шага от имени
// владельца роя в обход сессии и проверки прав. Обход живёт только в
// тестах: боевой код отвечает через AnswerSwarmRequest, где права
// перепроверяются в той же транзакции. Автор записывается, поэтому аудит
// answered_by в тестах полный. Идентичный повтор принимается.
func (s *Store) AnswerPipelineVisit(ctx context.Context, swarmID, legacyRunID string, sequence int64, answer string) error {
	if legacyRunID != swarmID {
		return ErrPipelineVisitConflict
	}
	if !validPipelineField(answer) || sequence < 1 {
		return errors.New("invalid pipeline answer")
	}
	return s.WithTx(ctx, func(ctx context.Context) error {
		w, err := s.GetSwarm(ctx, swarmID)
		if err != nil {
			return err
		}
		var id string
		err = s.conn(ctx).QueryRowContext(ctx, `SELECT id FROM swarm_requests WHERE swarm_id=? AND sequence=? AND origin='node'`, swarmID, sequence).Scan(&id)
		if err != nil {
			return ErrNotFound
		}
		req, err := s.GetSwarmRequest(ctx, swarmID, id)
		if err != nil {
			return err
		}
		if req.State == "answered" && req.Outcome != nil && *req.Outcome == answer {
			return nil
		}
		if req.State != "open" {
			return ErrPipelineVisitConflict
		}
		if validateAnswer(req, SwarmAnswer{Outcome: answer}) != nil {
			return errors.New("answer is not a human option")
		}
		if err := s.recordAnswer(ctx, req, w.OwnerID, answer, ""); err != nil {
			return err
		}
		return s.advanceNodeRequest(ctx, w, req, answer)
	})
}

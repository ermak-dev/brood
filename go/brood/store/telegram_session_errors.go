package store

import (
	"context"
	"database/sql"
)

const telegramSessionErrorsSchema = `CREATE TABLE telegram_session_errors (
    actor_uid TEXT NOT NULL REFERENCES managed_actors(uid) ON DELETE CASCADE,
    generation INTEGER NOT NULL,
    session_id TEXT NOT NULL,
    turn_id TEXT NOT NULL,
    PRIMARY KEY (actor_uid, generation, session_id, turn_id)
)`

func verifyTelegramSessionErrorsSchema(ctx context.Context, tx *sql.Tx) error {
	return checkSchema(ctx, tx, telegramSessionErrorsSchema)
}

// MarkTelegramSessionError records a final failure before any Bot API call.
func (s *Store) MarkTelegramSessionError(ctx context.Context, actorUID string, generation int64, sessionID, turnID string) (bool, error) {
	var first bool
	err := s.WithTx(ctx, func(ctx context.Context) error {
		db := s.conn(ctx)
		r, err := db.ExecContext(ctx, `INSERT INTO telegram_session_errors (actor_uid, generation, session_id, turn_id) VALUES (?,?,?,?) ON CONFLICT DO NOTHING`, actorUID, generation, sessionID, turnID)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil || n == 0 {
			return err
		}
		first = true
		_, err = db.ExecContext(ctx, `DELETE FROM telegram_session_errors WHERE actor_uid=? AND rowid < (SELECT rowid FROM telegram_session_errors WHERE actor_uid=? ORDER BY rowid DESC LIMIT 1 OFFSET 255)`, actorUID, actorUID)
		return err
	})
	return first && err == nil, err
}

package store

import (
	"context"
	"database/sql"
	"errors"
)

// TelegramArtifactReply addresses an exact artifact and its original author.
type TelegramArtifactReply struct {
	SwarmID   string
	ActorUID  string
	VersionID string
}

// RememberTelegramArtifactReply binds a Telegram message to the current link session.
func (s *Store) RememberTelegramArtifactReply(ctx context.Context, session string, messageID int64, route TelegramArtifactReply) error {
	return s.WithTx(ctx, func(ctx context.Context) error {
		db := s.conn(ctx)
		_, err := db.ExecContext(ctx, `INSERT INTO telegram_artifact_replies (session_id, message_id, swarm_id, actor, version_id) VALUES (?, ?, ?, ?, ?)`, session, messageID, route.SwarmID, route.ActorUID, route.VersionID)
		if err != nil {
			return err
		}
		// Связь старых ответов не должна расти без границ на одну привязку.
		_, err = db.ExecContext(ctx, `DELETE FROM telegram_artifact_replies WHERE session_id = ? AND message_id < (SELECT message_id FROM telegram_artifact_replies WHERE session_id = ? ORDER BY message_id DESC LIMIT 1 OFFSET 1023)`, session, session)
		return err
	})
}

// GetTelegramArtifactReply returns the binding within the supplied link session.
func (s *Store) GetTelegramArtifactReply(ctx context.Context, session string, messageID int64) (TelegramArtifactReply, error) {
	var route TelegramArtifactReply
	err := s.conn(ctx).QueryRowContext(ctx, `SELECT swarm_id, actor, version_id FROM telegram_artifact_replies WHERE session_id = ? AND message_id = ?`, session, messageID).Scan(&route.SwarmID, &route.ActorUID, &route.VersionID)
	if errors.Is(err, sql.ErrNoRows) {
		return route, ErrNotFound
	}
	return route, err
}

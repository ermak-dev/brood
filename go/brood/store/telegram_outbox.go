package store

import "context"

// Исходящие уведомления Telegram (#279, эпик #278): схема и ограждения —
// telegram_schema.go, запись уведомлений и отправку добавляет отправитель
// эпика.

// RecoverTelegramOutbox settles every interrupted Telegram attempt as
// unconfirmed: its sendMessage may have reached the chat, so the
// notification is neither sent again from the queue nor marked failed, and
// only an explicit retry attempts it again. The sender calls it at startup,
// before its first attempt, after the previous server has stopped.
func (s *Store) RecoverTelegramOutbox(ctx context.Context) error {
	_, err := s.conn(ctx).ExecContext(ctx, `UPDATE telegram_outbox SET state = 'unconfirmed', settled_attempts = attempts, updated_at = ? WHERE attempts > settled_attempts`,
		nowRFC3339())
	return err
}

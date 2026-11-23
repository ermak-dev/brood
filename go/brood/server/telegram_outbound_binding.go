package server

import "context"

type telegramOutboundBindingKey struct{}

// telegramOutboundBinding запрещает перенести уже подготовленное уведомление
// об ошибке на новую привязку или другому владельцу.
func (s *Server) telegramOutboundBinding(ctx context.Context, userID string, chatID int64) bool {
	expected, ok := ctx.Value(telegramOutboundBindingKey{}).(telegramChat)
	if !ok {
		return true
	}
	if userID != expected.userID || chatID != expected.chatID {
		return false
	}
	session, err := s.store.TelegramChatSession(ctx, userID, expected.botUsername, chatID, chatID)
	return err == nil && session == expected.session
}

package telegram

import "context"

// SendTyping shows the transient typing indicator for a chat.
func (c *Client) SendTyping(ctx context.Context, chatID int64) error {
	if chatID == 0 {
		return invalid("no chat")
	}
	params := struct {
		ChatID int64  `json:"chat_id"`
		Action string `json:"action"`
	}{chatID, "typing"}
	var ok bool
	if err := c.callJSON(ctx, "sendChatAction", params, 0, &ok); err != nil {
		return err
	}
	if !ok {
		return invalidResult("sendChatAction")
	}
	return nil
}

package telegram

// Chat types of the Bot API.
const (
	ChatPrivate    = "private"
	ChatGroup      = "group"
	ChatSupergroup = "supergroup"
	ChatChannel    = "channel"
)

// User is a Telegram user or bot, with the fields brood uses.
type User struct {
	ID        int64  `json:"id"`
	IsBot     bool   `json:"is_bot"`
	FirstName string `json:"first_name"`
	Username  string `json:"username,omitempty"`
}

// Chat is the chat of a message. Type is one of the Chat* constants.
type Chat struct {
	ID       int64  `json:"id"`
	Type     string `json:"type"`
	Username string `json:"username,omitempty"`
}

// Private reports whether the chat is a private chat with one user.
func (c Chat) Private() bool { return c.Type == ChatPrivate }

// Message is a chat message. From is nil for a message sent on behalf of a
// chat; Date is zero for a message the bot can no longer access.
type Message struct {
	MessageID      int64    `json:"message_id"`
	From           *User    `json:"from,omitempty"`
	Chat           Chat     `json:"chat"`
	Date           int64    `json:"date"`
	Text           string   `json:"text,omitempty"`
	ReplyToMessage *Message `json:"reply_to_message,omitempty"`
}

// CallbackQuery is a press of an inline keyboard button. Message is the
// message that carries the keyboard, nil when Telegram does not send it.
type CallbackQuery struct {
	ID      string   `json:"id"`
	From    User     `json:"from"`
	Message *Message `json:"message,omitempty"`
	Data    string   `json:"data,omitempty"`
}

// Update is one incoming update. At most one of Message and CallbackQuery is
// set; an update of another kind has neither. Malformed marks an update
// whose known fields did not parse: it keeps only its ID, so polling can
// confirm it and move on.
type Update struct {
	UpdateID      int64          `json:"update_id"`
	Message       *Message       `json:"message,omitempty"`
	CallbackQuery *CallbackQuery `json:"callback_query,omitempty"`
	Malformed     bool           `json:"-"`
}

// ReplyMarkup is the reply_markup of a sent message: an *InlineKeyboardMarkup
// or a *ForceReply. A nil ReplyMarkup, or a nil pointer of either kind, is no
// markup.
type ReplyMarkup interface {
	// encode checks the markup and returns its reply_markup value, nil for
	// none.
	encode() (any, error)
}

// ForceReply asks the Telegram client to open a reply to the message, as if
// the user chose Reply. Placeholder is the hint in the empty input field, at
// most 64 characters (TextLength), empty for none.
type ForceReply struct {
	Placeholder string
}

// InlineKeyboardMarkup is an inline keyboard of callback buttons, in rows. It
// has at least one row; a nil *InlineKeyboardMarkup is no keyboard.
type InlineKeyboardMarkup struct {
	InlineKeyboard [][]InlineKeyboardButton `json:"inline_keyboard"`
}

// InlineKeyboardButton is a button that sends CallbackData back to the bot.
type InlineKeyboardButton struct {
	Text         string `json:"text"`
	CallbackData string `json:"callback_data"`
}

// Document is a file to send: Name is the file name shown in the chat.
type Document struct {
	Name    string
	Content []byte
	Caption string
}

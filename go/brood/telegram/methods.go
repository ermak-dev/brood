package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	// MaxPollWait caps the long-poll wait: NAT and proxies often cut a
	// connection idle for a minute.
	MaxPollWait = 50 * time.Second
	// MaxUpdates is the Bot API cap on updates per getUpdates.
	MaxUpdates = 100
	// MaxDocumentBytes bounds a document: 64 times the largest request text
	// the store keeps (16 KiB, store.MaxSwarmRequestTextBytes), small enough
	// to upload within DefaultTimeout.
	MaxDocumentBytes = 1 << 20
)

// Bot API limits checked before sending.
const (
	maxButtons      = 100
	maxCallbackData = 64
	maxAnswerText   = 200
	maxQueryID      = 256
)

// pollKinds are the update kinds the package parses. Telegram keeps the list
// between calls; GetUpdates sends it every time.
var pollKinds = []string{"message", "callback_query"}

var fileName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// GetMe returns the bot of the token: it proves the token and gives the bot
// username. A bad token is ErrUnauthorized.
func (c *Client) GetMe(ctx context.Context) (User, error) {
	var me User
	if err := c.callJSON(ctx, "getMe", struct{}{}, 0, &me); err != nil {
		return User{}, err
	}
	if me.ID <= 0 || !me.IsBot || me.Username == "" {
		return User{}, invalidResult("getMe")
	}
	return me, nil
}

// Poll is one getUpdates request.
type Poll struct {
	// Offset is the first update ID to return, zero for the earliest
	// unconfirmed one. It confirms every earlier update and Telegram forgets
	// them, so the caller advances it (NextOffset) only past updates it has
	// handled.
	Offset int64
	// Limit caps the updates returned, 1..MaxUpdates, zero for MaxUpdates.
	Limit int
	// Wait is the long-poll wait, 0..MaxPollWait, zero for a short poll. The
	// Bot API takes whole seconds, so a fraction is a local refusal. It
	// extends the call deadline.
	Wait time.Duration
}

// GetUpdates long-polls for message and callback query updates. A 409
// (ErrConflict) means another poller or a webhook of the same bot. On
// ErrTooLarge the batch did not fit the client's bound on a response, and
// Telegram still holds it: the caller polls again from the same offset with
// Limit 1, since a single update always fits, and goes on from there.
func (c *Client) GetUpdates(ctx context.Context, p Poll) ([]Update, error) {
	switch {
	case p.Offset < 0:
		return nil, invalid("update offset %d is negative", p.Offset)
	case p.Limit < 0 || p.Limit > MaxUpdates:
		return nil, invalid("update limit %d is outside 1..%d", p.Limit, MaxUpdates)
	case p.Wait < 0 || p.Wait > MaxPollWait:
		return nil, invalid("long-poll wait %s is outside 0..%s", p.Wait, MaxPollWait)
	case p.Wait%time.Second != 0:
		return nil, invalid("long-poll wait %s is not a whole number of seconds", p.Wait)
	}
	limit := p.Limit
	if limit == 0 {
		limit = MaxUpdates
	}
	params := struct {
		Offset         int64    `json:"offset,omitempty"`
		Limit          int      `json:"limit"`
		Timeout        int      `json:"timeout"`
		AllowedUpdates []string `json:"allowed_updates"`
	}{p.Offset, limit, int(p.Wait / time.Second), pollKinds}
	var raw []json.RawMessage
	if err := c.callJSON(ctx, "getUpdates", params, p.Wait, &raw); err != nil {
		return nil, err
	}
	updates, ok := parseUpdates(raw)
	if !ok {
		return nil, invalidResult("getUpdates")
	}
	return updates, nil
}

// parseUpdates decodes each update on its own. An update whose known fields
// do not parse keeps its ID and is marked Malformed: polling must move past
// it, or one bad update would stop the bot. An update without an ID fails
// the batch, since the offset cannot pass it safely.
func parseUpdates(raw []json.RawMessage) ([]Update, bool) {
	updates := make([]Update, 0, len(raw))
	for _, r := range raw {
		var id struct {
			UpdateID *int64 `json:"update_id"`
		}
		if json.Unmarshal(r, &id) != nil || id.UpdateID == nil || *id.UpdateID < 0 {
			return nil, false
		}
		var u Update
		if json.Unmarshal(r, &u) != nil {
			u = Update{UpdateID: *id.UpdateID, Malformed: true}
		}
		updates = append(updates, u)
	}
	return updates, true
}

// NextOffset returns the offset that confirms every update in updates: one
// past the largest ID, never below current. The Bot API asks for an offset
// one past the highest ID received so far, and a batch it answers from the
// offset holds no lower ID; keeping the larger offset also lets a caller
// apply an older batch again without moving polling back.
//
// After a week without updates Telegram picks the next update ID at random,
// and it may fall below current. The Bot API server then takes the offset for
// one in the future, ignores it and answers from the earliest pending update:
// the same batch on every poll while the offset stays above it. NextOffset
// keeps current for such a batch, so a caller that sees a non-empty batch
// leave the offset unchanged handles the batch and goes on from
// NextOffset(0, updates).
func NextOffset(current int64, updates []Update) int64 {
	for _, u := range updates {
		if u.UpdateID >= current {
			current = u.UpdateID + 1
		}
	}
	return current
}

// SendMessage sends plain text, at most MessageLimit characters (TextLength),
// with an optional inline keyboard. A longer text is split (SplitText) or
// sent as a document. The message has no link preview: Telegram would fetch
// the URLs of the text for one, and mirrored text may hold URLs with tokens.
// An uncertain failure may have delivered the message.
func (c *Client) SendMessage(ctx context.Context, chatID int64, text string, markup *InlineKeyboardMarkup) (Message, error) {
	if chatID == 0 {
		return Message{}, invalid("no chat")
	}
	if err := checkText("message text", text, MessageLimit); err != nil {
		return Message{}, err
	}
	if err := markup.check(); err != nil {
		return Message{}, err
	}
	// Превью ссылки Telegram строит, сам открывая URL из текста, а в
	// зеркальном тексте URL может нести токен.
	type linkPreview struct {
		IsDisabled bool `json:"is_disabled"`
	}
	params := struct {
		ChatID      int64                 `json:"chat_id"`
		Text        string                `json:"text"`
		LinkPreview linkPreview           `json:"link_preview_options"`
		ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
	}{chatID, text, linkPreview{IsDisabled: true}, markup}
	return sent("sendMessage", func(out *Message) error {
		return c.callJSON(ctx, "sendMessage", params, 0, out)
	})
}

// SendDocument uploads doc as a file with an optional caption of at most
// CaptionLimit characters and an inline keyboard. An uncertain failure may
// have delivered the document.
func (c *Client) SendDocument(ctx context.Context, chatID int64, doc Document, markup *InlineKeyboardMarkup) (Message, error) {
	if chatID == 0 {
		return Message{}, invalid("no chat")
	}
	if !fileName.MatchString(doc.Name) {
		return Message{}, invalid("document name must be 1 to 100 letters, digits, dots, dashes or underscores, starting with a letter or digit")
	}
	if len(doc.Content) == 0 || len(doc.Content) > MaxDocumentBytes {
		return Message{}, invalid("document must be 1 to %d bytes", MaxDocumentBytes)
	}
	if doc.Caption != "" {
		if err := checkText("caption", doc.Caption, CaptionLimit); err != nil {
			return Message{}, err
		}
	}
	if err := markup.check(); err != nil {
		return Message{}, err
	}
	body, contentType, err := documentForm(chatID, doc, markup)
	if err != nil {
		return Message{}, invalid("cannot encode the document")
	}
	return sent("sendDocument", func(out *Message) error {
		return c.call(ctx, "sendDocument", contentType, body, 0, out)
	})
}

// sent runs a send and checks that its result is a message.
func sent(method string, send func(*Message) error) (Message, error) {
	var m Message
	if err := send(&m); err != nil {
		return Message{}, err
	}
	if m.MessageID <= 0 {
		return Message{}, invalidResult(method)
	}
	return m, nil
}

// documentForm encodes sendDocument parameters as multipart/form-data.
func documentForm(chatID int64, doc Document, markup *InlineKeyboardMarkup) ([]byte, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fields := [][2]string{{"chat_id", strconv.FormatInt(chatID, 10)}}
	if doc.Caption != "" {
		fields = append(fields, [2]string{"caption", doc.Caption})
	}
	if markup != nil {
		b, err := json.Marshal(markup)
		if err != nil {
			return nil, "", err
		}
		fields = append(fields, [2]string{"reply_markup", string(b)})
	}
	for _, f := range fields {
		if err := w.WriteField(f[0], f[1]); err != nil {
			return nil, "", err
		}
	}
	part, err := w.CreateFormFile("document", doc.Name)
	if err != nil {
		return nil, "", err
	}
	if _, err := part.Write(doc.Content); err != nil {
		return nil, "", err
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}

// EditMessageReplyMarkup replaces the inline keyboard of a message; a nil
// markup removes it.
func (c *Client) EditMessageReplyMarkup(ctx context.Context, chatID, messageID int64, markup *InlineKeyboardMarkup) error {
	if chatID == 0 || messageID <= 0 {
		return invalid("no chat or message")
	}
	if err := markup.check(); err != nil {
		return err
	}
	params := struct {
		ChatID      int64                 `json:"chat_id"`
		MessageID   int64                 `json:"message_id"`
		ReplyMarkup *InlineKeyboardMarkup `json:"reply_markup,omitempty"`
	}{chatID, messageID, markup}
	var result json.RawMessage
	if err := c.callJSON(ctx, "editMessageReplyMarkup", params, 0, &result); err != nil {
		return err
	}
	// Telegram отвечает правленым сообщением, а иногда просто true.
	if bytes.Equal(bytes.TrimSpace(result), []byte("true")) {
		return nil
	}
	var m Message
	if json.Unmarshal(result, &m) != nil || m.MessageID <= 0 {
		return invalidResult("editMessageReplyMarkup")
	}
	return nil
}

// AnswerCallbackQuery acknowledges a button press, with an optional
// notification text of at most 200 characters.
func (c *Client) AnswerCallbackQuery(ctx context.Context, queryID, text string) error {
	if queryID == "" || len(queryID) > maxQueryID || !utf8.ValidString(queryID) {
		return invalid("callback query ID must be 1 to %d bytes of UTF-8", maxQueryID)
	}
	if text != "" {
		if err := checkText("callback answer text", text, maxAnswerText); err != nil {
			return err
		}
	}
	params := struct {
		ID   string `json:"callback_query_id"`
		Text string `json:"text,omitempty"`
	}{queryID, text}
	var ok bool
	if err := c.callJSON(ctx, "answerCallbackQuery", params, 0, &ok); err != nil {
		return err
	}
	if !ok {
		return invalidResult("answerCallbackQuery")
	}
	return nil
}

// checkText refuses text Telegram would refuse: not UTF-8, blank, or longer
// than limit characters (TextLength).
func checkText(what, text string, limit int) error {
	switch {
	case !utf8.ValidString(text):
		return invalid("%s is not UTF-8", what)
	case strings.TrimSpace(text) == "":
		return invalid("%s is empty", what)
	case TextLength(text) > limit:
		return invalid("%s exceeds %d characters", what, limit)
	}
	return nil
}

// check refuses a keyboard Telegram would refuse. A keyboard needs a row: a
// nil InlineKeyboard goes as {"inline_keyboard":null}, which Telegram
// refuses, and no keyboard is a nil markup.
func (m *InlineKeyboardMarkup) check() error {
	if m == nil {
		return nil
	}
	if len(m.InlineKeyboard) == 0 {
		return invalid("inline keyboard has no rows: pass no keyboard instead")
	}
	buttons := 0
	for _, row := range m.InlineKeyboard {
		if len(row) == 0 {
			return invalid("inline keyboard has an empty row")
		}
		for _, b := range row {
			buttons++
			if err := checkText("button text", b.Text, MessageLimit); err != nil {
				return err
			}
			if b.CallbackData == "" || len(b.CallbackData) > maxCallbackData || !utf8.ValidString(b.CallbackData) {
				return invalid("callback data must be 1 to %d bytes of UTF-8", maxCallbackData)
			}
		}
	}
	if buttons > maxButtons {
		return invalid("inline keyboard has %d buttons, more than %d", buttons, maxButtons)
	}
	return nil
}

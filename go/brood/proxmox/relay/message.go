package relay

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Канал сообщений роя: тот же гостевой relay-транспорт, что и терминал,
// но с собственным типом SSH-канала. Терминальный поток двунаправленно
// копируется в ttyd; поток сообщений — обмен завершёнными NDJSON-кадрами
// ccmsg с полным текстом в ExtraData канала и ответом в SSH-канале.
const messageChannel = "brood-swarm-msg"

// MaxMessageBytes ограничивает и запрос, и ответ: сервер не буферизует
// произвольный объём от гостя, а гость — от соседа через сервер.
const MaxMessageBytes = 128 * 1024

// MaxMessagesInFlight — предел одновременно обрабатываемых сообщений роя
// на одного гостя: каждый канал держит горутину и запросы к хранилищу.
const MaxMessagesInFlight = 8

var errMessageTooLarge = errors.New("swarm message exceeds size limit")

// MessageRequest — один запрос гостя к соседу по рою. Проверку адреса
// (чей это актор, тот ли рой) делает сервер до передачи запроса дальше.
type MessageRequest struct {
	// DeliveryID is server-owned journal metadata, sent only after capability
	// negotiation. A guest cannot choose it for a routed peer message.
	DeliveryID string `json:"deliveryId,omitempty"`
	// JournalID binds the operation to the receiver's retained storage identity.
	JournalID string `json:"journalId,omitempty"`
	// Replay is a read-only probe of an uncertain attempt, never permission to send.
	Replay  bool   `json:"replay,omitempty"`
	To      string `json:"to"`      // имя актора-адресата в рое отправителя
	Body    string `json:"body"`    // непустой текст; конверт надстроит гость
	From    string `json:"from"`    // имя актора-отправителя
	WaitSec int    `json:"waitSec"` // ждать простоя адресата столько секунд
}

// MessageResponse — вердикт доставки: текст в одном поле, без вложенных
// структур, чтобы и сервер, и гость показывали его одинаково.
//
// NotSent (#244) — гость отказал до записи в сессию Claude (нет единственной
// живой сессии, нет ключа peer, helper не запустился): сообщение заведомо не
// ушло, и повтор безопасен. Без поля (false; прежний гость его не пишет) отказ
// означает неизвестный исход: сообщение могло дойти, повтор дал бы дубль.
type MessageResponse struct {
	// Duplicate acknowledges an exact delivery already confirmed in the durable journal.
	Duplicate bool   `json:"duplicate,omitempty"`
	OK        bool   `json:"ok"`
	Verdict   string `json:"verdict"`
	NotSent   bool   `json:"notSent,omitempty"`
}

// encodeMessageFrame собирает один NDJSON-кадр запроса. Предел проверяется
// до кодирования: сервер не буферизует произвольный объём от гостя.
func encodeMessageFrame(req MessageRequest) ([]byte, error) {
	if err := validateMessageDeliveryMetadata(req); err != nil {
		return nil, err
	}
	if req.To == "" || req.From == "" || req.Body == "" {
		return nil, errors.New("swarm message requires to, from and body")
	}
	if req.WaitSec < 0 || req.WaitSec > 300 {
		return nil, errors.New("waitSec must be between 0 and 300 seconds")
	}
	frame, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("swarm message encoding failed: %w", err)
	}
	if len(frame) > MaxMessageBytes {
		return nil, errMessageTooLarge
	}
	return frame, nil
}

// decodeMessageFrame разбирает кадр запроса с теми же границами.
func decodeMessageFrame(frame []byte) (MessageRequest, error) {
	if len(frame) > MaxMessageBytes {
		return MessageRequest{}, errMessageTooLarge
	}
	var req MessageRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return MessageRequest{}, errors.New("invalid swarm message frame")
	}
	if req.To == "" || req.From == "" || req.Body == "" {
		return MessageRequest{}, errors.New("swarm message requires to, from and body")
	}
	if req.WaitSec < 0 || req.WaitSec > 300 {
		return MessageRequest{}, errors.New("waitSec must be between 0 and 300 seconds")
	}
	if err := validateMessageDeliveryMetadata(req); err != nil {
		return MessageRequest{}, err
	}
	return req, nil
}

func validateMessageDeliveryMetadata(req MessageRequest) error {
	if req.DeliveryID == "" {
		if req.JournalID != "" || req.Replay {
			return errors.New("delivery metadata requires a stable delivery ID")
		}
		return nil
	}
	if !validMessageJournalID(req.JournalID) {
		return errors.New("stable delivery requires a retained journal identity")
	}
	return nil
}

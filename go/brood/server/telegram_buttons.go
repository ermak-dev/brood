package server

// Кнопки ответа Telegram (#283, эпик #278). Уведомление запроса получателю,
// который вправе управлять роем, несёт кнопки, пока запрос открыт: вариант
// выбора — кнопкой, confirm — Yes и No, на текстовый запрос отвечает reply на
// его сообщение, которое открывает поле ответа (force_reply, #298), или
// /answer <номер> <текст>. Выбор, чьи варианты Telegram
// кнопками не покажет, уходит без кнопок. callback_data несёт номер
// уведомления в outbox и индекс варианта, а не текст варианта: Bot API
// держит её в 64 байтах, а подпись варианта длиннее. Закрытый запрос
// показывает одну кнопку состояния: кто ответил и когда. Права здесь не
// проверяются: их проверяет транзакция ответа.

import (
	"strconv"
	"strings"
	"time"
	"unicode"

	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

const (
	// telegramAnswerPrefix — кнопка варианта: a:<уведомление>:<индекс>.
	telegramAnswerPrefix = "a:"
	// telegramStatusPrefix — кнопка состояния закрытого запроса:
	// s:<уведомление>; нажатие показывает состояние и ничего не пишет.
	telegramStatusPrefix = "s:"
	// telegramButtonOptions — индексы вариантов: клавиатура Bot API держит не
	// больше 100 кнопок.
	telegramButtonOptions = 100
	// telegramStatusLimit — длина надписи кнопки состояния: длинную Telegram
	// обрезает сам, и смысл уходит.
	telegramStatusLimit = 100
	// telegramStateTime — момент ответа в состоянии запроса.
	telegramStateTime = "2006-01-02 15:04 UTC"
	// telegramAnswerPlaceholder — подсказка в пустом поле ответа текстового
	// запроса перед номером уведомления; Bot API держит её в 64 знаках, и
	// номер int64 в них помещается.
	telegramAnswerPlaceholder = "Answer to request "
)

// telegramButton — разобранная callback_data: уведомление и вариант или
// кнопка состояния.
type telegramButton struct {
	notification int64
	option       int
	status       bool
}

func telegramAnswerData(notification int64, option int) string {
	return telegramAnswerPrefix + strconv.FormatInt(notification, 10) + ":" + strconv.Itoa(option)
}

func telegramStatusData(notification int64) string {
	return telegramStatusPrefix + strconv.FormatInt(notification, 10)
}

// parseTelegramButton разбирает callback_data кнопок brood. Принимается только
// каноническая форма, которую пишет сервер: номер уведомления положителен,
// индекс варианта в пределах клавиатуры, без знаков и ведущих нулей.
func parseTelegramButton(data string) (telegramButton, bool) {
	if rest, ok := strings.CutPrefix(data, telegramStatusPrefix); ok {
		id, ok := telegramCanonical(rest)
		return telegramButton{notification: id, status: true}, ok && id > 0
	}
	rest, ok := strings.CutPrefix(data, telegramAnswerPrefix)
	if !ok {
		return telegramButton{}, false
	}
	head, tail, ok := strings.Cut(rest, ":")
	if !ok {
		return telegramButton{}, false
	}
	id, idOK := telegramCanonical(head)
	option, optionOK := telegramCanonical(tail)
	if !idOK || !optionOK || id <= 0 || option >= telegramButtonOptions {
		return telegramButton{}, false
	}
	return telegramButton{notification: id, option: int(option)}, true
}

// telegramCanonical — неотрицательное число в десятичной записи без знака и
// ведущих нулей.
func telegramCanonical(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil && n >= 0 && strconv.FormatInt(n, 10) == s
}

// telegramRequestKeyboard — кнопки ответа на запрос уведомления notification:
// кнопка на вариант выбора по строке, Yes и No у confirm; у текстового
// запроса кнопок нет. Нет их и у выбора, который Telegram кнопками не
// покажет (telegramButtonLabels): клавиатуру он отверг бы вместе с
// уведомлением.
func telegramRequestKeyboard(notification int64, r store.SwarmRequest) *telegram.InlineKeyboardMarkup {
	var rows [][]telegram.InlineKeyboardButton
	switch r.Kind {
	case "choice":
		if !telegramButtonLabels(r.Options) {
			return nil
		}
		for i, o := range r.Options {
			rows = append(rows, []telegram.InlineKeyboardButton{{Text: telegramButtonText(o.Label), CallbackData: telegramAnswerData(notification, i)}})
		}
	case "confirm":
		rows = append(rows, []telegram.InlineKeyboardButton{
			{Text: "Yes", CallbackData: telegramAnswerData(notification, 0)},
			{Text: "No", CallbackData: telegramAnswerData(notification, 1)},
		})
	}
	if len(rows) == 0 {
		return nil
	}
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: rows}
}

// telegramAnswerField — поле ответа текстового запроса уведомления
// notification (force_reply, #298): клиент Telegram сам открывает reply на
// сообщение запроса, а reply записывается ответом на запрос того сообщения,
// на которое он сделан (textReply, reply_to_message). Подсказка называет
// запрос тем же номером, что /answer в тексте сообщения; клиент показывает её
// в пустом поле, пока открыт reply, который открыло это сообщение. Запрос
// ответа она не выбирает: его выбирает сообщение, на которое сделан reply, и
// это сообщение клиент показывает над полем. Новое сообщение с полем ответа,
// например второго открытого запроса, открывает reply на себя со своей
// подсказкой. На запрос с кнопками отвечают кнопками, поля у него нет.
func telegramAnswerField(notification int64, r store.SwarmRequest) *telegram.ForceReply {
	if r.Kind != "text" {
		return nil
	}
	return &telegram.ForceReply{Placeholder: telegramAnswerPlaceholder + strconv.FormatInt(notification, 10)}
}

// telegramButtonLabels: варианты выбора помещаются в клавиатуру так, как её
// проверяет клиент Bot API: не больше telegramButtonOptions кнопок, у каждой
// подпись не пуста после пробелов и не длиннее сообщения. Граф и хранилище
// принимают подпись " " и узел с сотней вариантов, Telegram — нет.
func telegramButtonLabels(options []store.RequestOption) bool {
	if len(options) == 0 || len(options) > telegramButtonOptions {
		return false
	}
	for _, o := range options {
		label := telegramButtonText(o.Label)
		if strings.TrimSpace(label) == "" || telegram.TextLength(label) > telegram.MessageLimit {
			return false
		}
	}
	return true
}

// telegramButtonText — надпись кнопки варианта: подпись, починенная до
// UTF-8, как текст уведомления. Ответом остаётся подпись варианта по его
// индексу, а не надпись.
func telegramButtonText(label string) string {
	return strings.ToValidUTF8(label, "?")
}

// telegramStatusKeyboard — одна кнопка состояния вместо кнопок ответа.
func telegramStatusKeyboard(notification int64, state string) *telegram.InlineKeyboardMarkup {
	return &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{
		{{Text: telegramClipText(state, telegramStatusLimit), CallbackData: telegramStatusData(notification)}},
	}}
}

// telegramButtonAnswer — ответ, который хранилище примет за нажатый вариант:
// исход маршрута у узлового запроса, подпись варианта у выбора актора, yes
// или no у confirm. У текстового запроса и индекса вне вариантов ответа нет.
func telegramButtonAnswer(r store.SwarmRequest, option int) (outcome, response string, ok bool) {
	switch {
	case r.Kind == "confirm" && r.Origin == "actor" && option >= 0 && option <= 1:
		return "", []string{"yes", "no"}[option], true
	case r.Kind != "choice" || option < 0 || option >= len(r.Options):
		return "", "", false
	case r.Origin == "node":
		return r.Options[option].Outcome, "", true
	}
	return "", r.Options[option].Label, true
}

// telegramRequestState — состояние запроса для чата: кто и когда ответил и
// выбранный вариант (текст ответа — нет, он длинный), отмена или открытый
// запрос. author — имя автора ответа.
func telegramRequestState(r store.SwarmRequest, author string) string {
	switch r.State {
	case "cancelled":
		return "This request was cancelled."
	case "answered":
	default:
		return "This request is open."
	}
	state := "Answered by " + author
	if r.AnsweredAt != nil {
		if at, err := time.Parse(time.RFC3339Nano, *r.AnsweredAt); err == nil {
			state += " at " + at.UTC().Format(telegramStateTime)
		}
	}
	if label, ok := r.OptionLabel(); ok {
		state += ": " + label
	} else if r.Kind != "text" && r.Response != nil && *r.Response != "" {
		state += ": " + *r.Response
	}
	return state + "."
}

// telegramAnswerArgs разбирает /answer <номер> <текст>: номер уведомления
// запроса и текст ответа с его строками. Имя команды — в любом регистре, как
// у команд чата (#284). Без номера или текста — отказ.
func telegramAnswerArgs(text string) (notification int64, answer string, ok bool) {
	head, rest := telegramField(text)
	name, _, _ := strings.Cut(strings.TrimPrefix(head, "/"), "@")
	if !strings.HasPrefix(head, "/") || !telegramAnswerName(name) {
		return 0, "", false
	}
	number, rest := telegramField(rest)
	id, ok := telegramCanonical(number)
	answer = strings.TrimSpace(rest)
	if !ok || id <= 0 || answer == "" {
		return 0, "", false
	}
	return id, answer, true
}

// telegramAnswerName: имя команды name — /answer в любом регистре. Поллер
// по нему отдаёт команду ответам, а не командам чата, которые имя тоже
// сравнивают без регистра.
func telegramAnswerName(name string) bool {
	return strings.EqualFold(name, "answer")
}

// telegramField — первое слово s после пробелов и остаток после него.
func telegramField(s string) (string, string) {
	s = strings.TrimLeftFunc(s, unicode.IsSpace)
	if i := strings.IndexFunc(s, unicode.IsSpace); i >= 0 {
		return s[:i], s[i:]
	}
	return s, ""
}

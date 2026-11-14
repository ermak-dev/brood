package server

// Текст уведомлений Telegram (#282). Хранилище ставит в outbox событие —
// рой и объект, — а текст собирает сервер в момент отправки: по событию и по
// неизменяемым объектам, которые оно называет (вопрос, варианты, материал и
// ответ запроса, задача прохода и причина отказа шага не меняются после
// записи). Текст простой, без parse_mode:
// вопрос и материал актора идут как есть, без разметки, которую Telegram
// стал бы разбирать. Уведомление открытого запроса получателю, который вправе
// управлять роем, несёт кнопки ответа (#283, telegram_buttons.go).

import (
	"net/url"
	"strconv"
	"strings"

	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

// telegramFacts — что отправитель прочёл о событии в базе: имя роя,
// определение его снимка, запрос события, шаг события (первый шаг начатого
// прохода с его задачей, отказанный шаг с причиной) и имена спросившего
// актора и автора ответа.
type telegramFacts struct {
	swarm      string
	definition string
	request    *store.SwarmRequest
	step       *store.PipelineVisit
	actor      string
	answeredBy string
}

// telegramNotice — сообщение уведомления: текст, а когда он длиннее
// предела сообщения Bot API, — один документ с полным текстом и подписью;
// markup — кнопки ответа, nil — без них.
type telegramNotice struct {
	text   string
	doc    *telegram.Document
	markup *telegram.InlineKeyboardMarkup
}

// telegramDocumentNote — подпись документа говорит, где полный текст.
const telegramDocumentNote = "The full text is in the attached file."

// renderTelegramNotice собирает сообщение уведомления n по фактам f. origin
// — origin панели (BROOD_PANEL_ORIGIN); пусто — сообщение без ссылки.
func renderTelegramNotice(n store.TelegramNotification, f telegramFacts, origin string) telegramNotice {
	answerable := telegramAnswerable(n, f)
	var markup *telegram.InlineKeyboardMarkup
	if answerable {
		markup = telegramRequestKeyboard(n.ID, *f.request)
	}
	head, sections := telegramNoticeBody(n.Kind, n.Event, f)
	hint := telegramNoticeHint(n, f, answerable, markup != nil)
	sections = append(sections, hint)
	link := ""
	if origin != "" {
		// Сервер отдаёт панель только в корне, а страницу выбирает фрагмент:
		// панель маршрутизирует по hash (createHashRouter, #194).
		link = "Open in brood: " + origin + "/#/swarms/" + url.PathEscape(n.Event.Swarm)
	}
	head = strings.ToValidUTF8(head, "?")
	parts := []string{head}
	for _, section := range sections {
		if section != "" {
			parts = append(parts, strings.ToValidUTF8(section, "?"))
		}
	}
	if link != "" {
		parts = append(parts, link)
	}
	text := strings.Join(parts, "\n\n")
	if telegram.TextLength(text) <= telegram.MessageLimit {
		return telegramNotice{text: text, markup: markup}
	}
	tail := "\n\n" + telegramDocumentNote
	if answerable && hint != "" {
		// Тот, кто вправе ответить, узнаёт из подписи, как: номер для /answer
		// не прячется в файле.
		tail = "\n\n" + hint + tail
	}
	if link != "" {
		tail += "\n\n" + link
	}
	caption := telegramClipText(head, telegram.CaptionLimit-telegram.TextLength(tail)) + tail
	return telegramNotice{doc: &telegram.Document{
		Name:    "brood-notification-" + strconv.FormatInt(n.ID, 10) + ".txt",
		Content: []byte(text),
		Caption: telegramClipText(caption, telegram.CaptionLimit),
	}, markup: markup}
}

// telegramAnswerable: уведомление открывает запрос, он ещё открыт, и
// получатель вправе управлять роем в момент попытки — ему уведомление
// предлагает ответить. Viewer кнопок не получает; ответ всё равно
// перепроверяет права в своей транзакции.
func telegramAnswerable(n store.TelegramNotification, f telegramFacts) bool {
	return n.Manager && f.request != nil && f.request.State == "open" && telegramOpensRequest(n.Kind)
}

// telegramNoticeHint — последний раздел уведомления открытого запроса: как
// ответить. Тому, кто вправе ответить (answerable), — кнопками (buttons —
// они есть), reply или /answer, а выбору, который Telegram кнопками не
// покажет, — в brood. Прочим вопрос актора называет ожидание, а не велит
// отвечать; узловой запрос и прочие виды подсказки не несут.
func telegramNoticeHint(n store.TelegramNotification, f telegramFacts, answerable, buttons bool) string {
	switch {
	case !telegramOpensRequest(n.Kind) || f.request == nil:
		return ""
	case !answerable && n.Kind == "actor_question":
		return telegramAnswerHint(f.request)
	case !answerable:
		return ""
	case n.Kind == "actor_question":
		return telegramManagerHint(n.ID, f.request, buttons)
	case buttons:
		return "Choose an option: answer with a button below."
	}
	return "Choose an option in brood: Telegram cannot show these options as buttons."
}

// telegramNoticeBody — первая строка уведомления вида kind и его разделы.
func telegramNoticeBody(kind string, ev store.TelegramEvent, f telegramFacts) (string, []string) {
	swarm := `Swarm "` + f.swarm + `": `
	switch kind {
	case "request_opened":
		return swarm + "node " + ev.Node + " asks the operator (step " + strconv.FormatInt(ev.Sequence, 10) + ").",
			telegramRequestSections(f.request)
	case "actor_question":
		return swarm + "actor " + f.actor + " asks the operator.",
			telegramRequestSections(f.request)
	case "request_answered":
		return swarm + f.answeredBy + " answered the request of " + telegramAsker(ev, f) + ".",
			[]string{telegramQuestion(f.request), telegramAnswer(f.request)}
	case "answer_delivery":
		return swarm + "the answer to actor " + f.actor + " " + telegramDelivery(ev.Delivery),
			[]string{telegramQuestion(f.request), telegramAnswer(f.request)}
	case "pass_started":
		head := swarm + telegramPass(ev.Sequence) + " started"
		if f.definition != "" {
			head += " on workflow " + f.definition
		}
		var goal string
		if f.step != nil {
			goal = f.step.Goal
		}
		return head + " at node " + ev.Node + ".", []string{telegramSection("Goal", goal)}
	case "pass_finished":
		return swarm + telegramPass(ev.Sequence) + " " + telegramPassEnd(ev) + ".", nil
	case "step_failed":
		head := swarm + "step " + strconv.FormatInt(ev.Sequence, 10) + " (node " + ev.Node + ") "
		if ev.State == "refused" {
			head += "was refused: its instruction was not sent."
		} else {
			head += "is unconfirmed: its outcome is unknown, and its instruction may have reached the actor."
		}
		var reason string
		if f.step != nil {
			reason = f.step.Detail
		}
		return head, []string{telegramSection("Reason", reason)}
	}
	return swarm + kind + ".", nil
}

// telegramPass — проход по номеру его первого шага; проход, открытый мимо
// запуска, номера не имеет.
func telegramPass(sequence int64) string {
	if sequence < 1 {
		return "the pass"
	}
	return "pass " + strconv.FormatInt(sequence, 10)
}

// telegramRequestSections — вопрос, варианты и материал запроса.
func telegramRequestSections(r *store.SwarmRequest) []string {
	if r == nil {
		return nil
	}
	var options []string
	for _, o := range r.Options {
		options = append(options, "- "+o.Label)
	}
	return []string{
		telegramSection("Question", r.Question),
		telegramSection("Options", strings.Join(options, "\n")),
		telegramSection("Material", r.Context),
	}
}

// telegramAnswerHint — какой ответ ждёт вопрос актора. Уведомление
// получает каждый читатель роя, и viewer ответить не вправе: подсказка
// называет ожидание, а не велит отвечать.
func telegramAnswerHint(r *store.SwarmRequest) string {
	if r == nil {
		return ""
	}
	switch r.Kind {
	case "choice":
		return "The actor waits for one of the options in brood."
	case "confirm":
		return "The actor waits for a yes or no in brood."
	}
	return "The actor waits for a text answer in brood."
}

// telegramManagerHint — как ответить на вопрос актора тому, кто вправе:
// кнопкой (buttons — они есть, иначе в brood), reply или /answer с номером
// уведомления notification.
func telegramManagerHint(notification int64, r *store.SwarmRequest, buttons bool) string {
	switch {
	case r.Kind == "choice" && buttons:
		return "The actor waits for one of the options: answer with a button below."
	case r.Kind == "choice":
		return "The actor waits for one of the options: answer in brood, Telegram cannot show them as buttons."
	case r.Kind == "confirm":
		return "The actor waits for a yes or no: answer with a button below."
	}
	return "The actor waits for a text answer: reply to this message or send /answer " + strconv.FormatInt(notification, 10) + " <your answer>."
}

func telegramQuestion(r *store.SwarmRequest) string {
	if r == nil {
		return ""
	}
	return telegramSection("Question", r.Question)
}

// telegramAnswer — ответ запроса: подпись выбранного варианта и свободный
// текст.
func telegramAnswer(r *store.SwarmRequest) string {
	if r == nil {
		return ""
	}
	var answer []string
	if label, ok := r.OptionLabel(); ok {
		answer = append(answer, label)
	}
	if r.Response != nil && *r.Response != "" {
		answer = append(answer, *r.Response)
	}
	return telegramSection("Answer", strings.Join(answer, "\n"))
}

// telegramAsker — кто спрашивал: узел графа или актор.
func telegramAsker(ev store.TelegramEvent, f telegramFacts) string {
	if f.request != nil && f.request.Origin == "actor" {
		return "actor " + f.actor
	}
	return "node " + ev.Node
}

// telegramDelivery — исход доставки ответа актору. Уведомление получает и
// viewer, а повторить доставку вправе только тот, кто управляет роем
// (swarmRequestDeliver): текст говорит, кто может, а не велит повторять.
func telegramDelivery(delivery string) string {
	switch delivery {
	case "delivered":
		return "was delivered."
	case "failed":
		return "could not be delivered; anyone who manages the swarm can retry the delivery in brood."
	}
	return "is unconfirmed: it may have reached the actor, and a retry may deliver it twice."
}

func telegramPassEnd(ev store.TelegramEvent) string {
	switch ev.Execution {
	case "succeeded":
		return "succeeded"
	case "stopped":
		return "was stopped"
	}
	if ev.Node != "" {
		return "failed at node " + ev.Node
	}
	return "failed"
}

// telegramSection — раздел с заголовком; пустой текст — без раздела.
func telegramSection(title, body string) string {
	if strings.TrimSpace(body) == "" {
		return ""
	}
	return title + ":\n" + body
}

// telegramClipText обрезает текст до limit знаков Bot API (TextLength) по
// границе символа и отмечает обрезку многоточием.
func telegramClipText(s string, limit int) string {
	if telegram.TextLength(s) <= limit {
		return s
	}
	const ellipsis = "..."
	budget := limit - len(ellipsis)
	n, cut := 0, len(s)
	for i, r := range s {
		units := 1
		if r > 0xFFFF {
			units = 2
		}
		if n+units > budget {
			cut = i
			break
		}
		n += units
	}
	return s[:cut] + ellipsis
}

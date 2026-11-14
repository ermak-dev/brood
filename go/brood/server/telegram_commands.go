package server

// Команды привязанного чата Telegram (#284, эпик #278). Чат действует от
// имени пользователя своей привязки, с его правами, и каждая команда идёт
// теми же методами сервера, что панель и bro: права — app, запуск и
// остановка — startSwarmPass и stopSwarm с сессией чата в транзакции записи,
// /use — SetTelegramSwarm так же, акторы — каталог команд через проверки
// маршрута POST /api/commands (commandPreflight). Своей логики решений у
// адаптера нет: он разбирает команду, проверяет право до подтверждения
// (чтобы не просить нажатия ради отказа) и переводит итог в текст.
// Окончательно право и сессию проверяет запись — кроме изменений акторов:
// запись жизненного цикла сессию чата не читает (как и веб-сессию POST
// /api/commands; проверка в самой записи — #299), поэтому /actor проверяет
// привязку перед командой каталога и после неё, а чат, чья привязка умерла,
// пока шла команда, узнаёт, что действие могло случиться.
//
// Рой, который пользователь не читает, не найден, как в API. Отказ несёт
// публичную причину отказа под ним (#276) или фиксированный текст; текст
// внутренней ошибки в чат не идёт. Ответ укладывается в предел сообщения Bot
// API: длинные списки — по страницам, строки обрезаны. Пакет молчит, кроме
// журнала внутренних сбоев: ответ — данные для поллера, который его шлёт.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

const (
	// telegramPageSize — элементов на странице списка, telegramLineLimit —
	// знаков в строке: 20 строк по 180 знаков с заголовком, ссылкой на
	// следующую страницу и подвалом меньше 4096 знаков сообщения.
	telegramPageSize  = 20
	telegramLineLimit = 180
	// telegramNameLimit — знаков имени роя, проекта или актора в тексте.
	telegramNameLimit = 100
	// telegramGoalLimit — знаков задачи прохода в подтверждении и в /swarm.
	telegramGoalLimit = 1000
	// telegramCommandTimeout ограничивает одну команду: поллер бота разбирает
	// обновления по одному, и долгая команда задерживает следующие.
	telegramCommandTimeout = time.Minute
	// telegramCheckTimeout ограничивает проверку привязки после команды: она
	// идёт и тогда, когда команда упёрлась в свой срок.
	telegramCheckTimeout = 5 * time.Second
)

// telegramHelp — ответ на /help.
var telegramHelp = `brood commands for this chat:
/swarms [page] - your swarms, their state and open requests
/swarm [swarm] - execution state, current step and open requests of a swarm
/use <swarm> - make a swarm the default of this chat; /use shows it
/requests [page] - open operator requests of the default swarm
/actors [swarm] [page] - managed actors of a swarm and their state
/start <swarm> <goal> - start a new pass of the swarm's pinned workflow
/stop <swarm> - stop the running pass of a swarm
/actor <swarm> <name> stop|start|update|reset - change a managed actor
/answer <number> <text> - answer a text request; the number is in the request message
/help - this list

Without a swarm, /swarm, /requests and /actors use the default swarm. Name a swarm by its name, as <project>/<name> when two of your swarms share a name, or by its id. /start, /stop and /actor ask for confirmation with a button, which expires after ` + telegramMinutes(telegramConfirmTTL) + `.

Answer a request with the buttons of its message, and a text request with a reply to its message or with /answer.`

// Ответы команд, общие для нескольких из них.
const (
	telegramReplyNoDefault   = "No default swarm: choose one with /use <swarm>."
	telegramReplyDefaultGone = "Your default swarm is no longer available to you: choose another with /use <swarm>."
	telegramReplyFailed      = "brood could not answer this command right now. Try again later."
	telegramReplyNotRun      = "brood could not run this action right now; nothing was done. Try again later."
	telegramReplyUnlinked    = "This chat is no longer linked to your brood account."
	telegramReplyNoSwarms    = "You have no swarms in brood."
	telegramReplyLate        = "This confirmation expired before its action could start; nothing was done. Send the command again."
)

// telegramActorActions — действия /actor, они же команды каталога.
var telegramActorActions = []string{"stop", "start", "update", "reset"}

// telegramUnknownReply — отказ неизвестной команды name.
func telegramUnknownReply(name string) string {
	return "Unknown command /" + telegramClipText(strings.ToValidUTF8(name, "?"), 32) + ". Send /help for the commands."
}

// telegramMinutes — длительность целыми минутами для текста.
func telegramMinutes(d time.Duration) string {
	n := int(d / time.Minute)
	if n == 1 {
		return "1 minute"
	}
	return strconv.Itoa(n) + " minutes"
}

// telegramReply — ответ на команду: текст и клавиатура подтверждения; пустой
// текст — ответа нет.
type telegramReply struct {
	text   string
	markup *telegram.InlineKeyboardMarkup
}

func telegramText(text string) telegramReply { return telegramReply{text: text} }

// telegramCommands — команды чатов над методами сервера.
type telegramCommands struct {
	server  *Server
	confirm *telegramConfirmations
	// panel — origin панели для ссылок, пусто — без ссылок.
	panel string
	// timeout — срок одной команды и одного нажатого действия, в работе
	// telegramCommandTimeout.
	timeout time.Duration
	// beforeWrite зовут команды после своих проверок, перед записью; nil в
	// работе. В нём проверки отвязывают чат.
	beforeWrite func()
}

func newTelegramCommands(s *Server, panel string, now func() time.Time) *telegramCommands {
	return &telegramCommands{server: s, confirm: newTelegramConfirmations(now), panel: panel, timeout: telegramCommandTimeout}
}

// writing — точка перед записью команды (beforeWrite).
func (c *telegramCommands) writing() {
	if c.beforeWrite != nil {
		c.beforeWrite()
	}
}

// run исполняет команду text привязанного чата chat и возвращает ответ.
func (c *telegramCommands) run(ctx context.Context, chat telegramChat, text string) telegramReply {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	name, _, ok := telegramCommand(text)
	if !ok {
		return telegramReply{}
	}
	name = strings.ToLower(name)
	var reply telegramReply
	var err error
	switch name {
	case "help":
		reply = telegramText(telegramHelp)
	case "swarms":
		reply, err = c.swarms(ctx, chat, text)
	case "swarm":
		reply, err = c.swarm(ctx, chat, text)
	case "use":
		reply, err = c.use(ctx, chat, text)
	case "requests":
		reply, err = c.requests(ctx, chat, text)
	case "actors":
		reply, err = c.actors(ctx, chat, text)
	case "start":
		reply, err = c.start(ctx, chat, text)
	case "stop":
		reply, err = c.stop(ctx, chat, text)
	case "actor":
		reply, err = c.actor(ctx, chat, text)
	default:
		reply = telegramText(telegramUnknownReply(name))
	}
	if err != nil {
		return telegramText(telegramFailed("/"+name, err))
	}
	return reply
}

// press решает, что делает нажатие кнопки с данными data в чате chat.
// Кнопка другого вида (ответ на запрос, #283) — не подтверждение: отказ без
// правки её клавиатуры; поллер такие кнопки отдаёт ответам, а не сюда.
// Действие, которое нажатие забрало, идёт в срок команды и только под той
// привязкой, под которой пришло нажатие: привязка, умершая после проверки
// поллера, пока нажатие ждало ответов Bot API, ничего не исполняет и ответа
// не получает. Начинается оно не позже срока подтверждения и отсрочки после
// него (telegramConfirmGrace), иначе подтверждение истекает. По окончании
// действие закрывает подтверждение: повтор нажатия его уже не исполнит.
func (c *telegramCommands) press(chat telegramChat, data string) telegramPress {
	if !strings.HasPrefix(data, telegramConfirmPrefix) {
		return telegramPress{answer: telegramPressInvalid}
	}
	id, yes, ok := parseTelegramConfirm(data)
	if !ok {
		return telegramPress{answer: telegramPressInvalid, clear: true}
	}
	p := c.confirm.press(chat, id, yes)
	if run := p.run; run != nil {
		p.run = func(ctx context.Context) string {
			defer c.confirm.finish(id)
			ctx, cancel := context.WithTimeout(ctx, c.timeout)
			defer cancel()
			live, err := c.linked(ctx, chat)
			if err != nil {
				slog.Error("telegram command failed", "command", "press", "err", err.Error())
				return telegramReplyNotRun
			}
			if !live {
				return ""
			}
			if !c.confirm.begin(id) {
				return telegramReplyLate
			}
			return run(ctx)
		}
	}
	return p
}

// telegramFailed пишет внутренний сбой команды в журнал сервера и
// возвращает общий отказ без его текста.
func telegramFailed(what string, err error) string {
	slog.Error("telegram command failed", "command", what, "err", err.Error())
	return telegramReplyFailed
}

// linked: привязка чата жива той сессией, под которой пришло его
// обновление, — через бот обновления, к этому чату и собеседнику. (false,
// nil) — привязка умерла или сделана заново; ошибка — проверить не удалось.
func (c *telegramCommands) linked(ctx context.Context, chat telegramChat) (bool, error) {
	session, err := c.server.store.TelegramChatSession(ctx, chat.userID, chat.botUsername, chat.chatID, chat.chatID)
	if err != nil {
		return false, err
	}
	return chat.linked() && session == chat.session, nil
}

// session — linked в транзакции записи (authorize запуска, остановки и
// /use, полномочие сессии команды каталога): сбой проверки — отказ.
func (c *telegramCommands) session(chat telegramChat) func(context.Context) bool {
	return func(ctx context.Context) bool {
		live, err := c.linked(ctx, chat)
		return err == nil && live
	}
}

// stillLinked — linked после команды, без отмены и срока команды и со своим
// коротким сроком: команда, упёршаяся в свой срок, не принимает его за
// отвязку и не теряет итог.
func (c *telegramCommands) stillLinked(ctx context.Context, chat telegramChat) (bool, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), telegramCheckTimeout)
	defer cancel()
	return c.linked(ctx, chat)
}

// visible: пользователь чата ещё видит рой. Проверки нажатия молчат о рое,
// который он больше не видит: его скрывает сама команда.
func (c *telegramCommands) visible(chat telegramChat, sw telegramSwarm) (bool, error) {
	return c.server.app.Can(chat.userID, app.ActionViewSwarm, app.SwarmRes(sw.ID))
}

// ─── Рои ───────────────────────────────────────────────────────────────────

// telegramSwarm — рой, который пользователь читает, с именем его проекта.
type telegramSwarm struct {
	app.Swarm
	project string
}

// label — рой для текста: имя в кавычках.
func (sw telegramSwarm) label() string { return telegramQuote(sw.Name) }

// telegramQuote — имя в кавычках Go, обрезанное до telegramNameLimit.
func telegramQuote(name string) string {
	return strconv.Quote(telegramClipText(name, telegramNameLimit))
}

// readable — рои, которые пользователь читает, по правилу списков панели:
// проекты пользователя (администратору — все) и их рои, где у него роль
// (администратору — все неудалённые). Порядок — проект, имя, id.
func (c *telegramCommands) readable(userID string) ([]telegramSwarm, error) {
	projects, err := c.server.app.ListProjectsFor(userID)
	if errors.Is(err, app.ErrNotAllowed) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []telegramSwarm
	for _, p := range projects {
		swarms, err := c.server.app.ListSwarmsFor(userID, p.ID)
		if errors.Is(err, app.ErrNotAllowed) || errors.Is(err, app.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, sw := range swarms {
			out = append(out, telegramSwarm{Swarm: sw, project: p.Name})
		}
	}
	slices.SortFunc(out, func(a, b telegramSwarm) int {
		if d := strings.Compare(a.project, b.project); d != 0 {
			return d
		}
		if d := strings.Compare(a.Name, b.Name); d != 0 {
			return d
		}
		return strings.Compare(a.ID, b.ID)
	})
	return out, nil
}

// telegramNotFound — отказ роя ref, которого нет среди роёв пользователя.
func telegramNotFound(ref string) string {
	return "No swarm " + telegramQuote(ref) + " among your swarms. Send /swarms for the list."
}

// resolve находит рой ref среди роёв пользователя: по id, по
// <проект>/<имя>, по имени. Отказ — текст для чата.
func (c *telegramCommands) resolve(userID, ref string) (telegramSwarm, string, error) {
	swarms, err := c.readable(userID)
	if err != nil {
		return telegramSwarm{}, "", err
	}
	for _, sw := range swarms {
		if sw.ID == ref {
			return sw, "", nil
		}
	}
	var named, qualified []telegramSwarm
	for _, sw := range swarms {
		if sw.project+"/"+sw.Name == ref {
			qualified = append(qualified, sw)
		}
		if sw.Name == ref {
			named = append(named, sw)
		}
	}
	for _, found := range [][]telegramSwarm{qualified, named} {
		switch len(found) {
		case 0:
			continue
		case 1:
			return found[0], "", nil
		}
		var names []string
		for _, sw := range found {
			names = append(names, telegramClipText(sw.project+"/"+sw.Name, telegramNameLimit))
		}
		return telegramSwarm{}, "Several of your swarms are named " + telegramQuote(ref) + ": name one as " + telegramOr(names) + ".", nil
	}
	return telegramSwarm{}, telegramNotFound(ref), nil
}

// telegramOr — «a, b or c».
func telegramOr(items []string) string {
	if len(items) < 2 {
		return strings.Join(items, "")
	}
	return strings.Join(items[:len(items)-1], ", ") + " or " + items[len(items)-1]
}

// target — рой команды: названный ref, без него — рой по умолчанию чата.
func (c *telegramCommands) target(ctx context.Context, chat telegramChat, ref string) (telegramSwarm, string, error) {
	if ref != "" {
		return c.resolve(chat.userID, ref)
	}
	link, err := c.server.store.GetTelegramLink(ctx, chat.userID)
	if err != nil {
		return telegramSwarm{}, "", err
	}
	if link.DefaultSwarmID == "" {
		return telegramSwarm{}, telegramReplyNoDefault, nil
	}
	swarms, err := c.readable(chat.userID)
	if err != nil {
		return telegramSwarm{}, "", err
	}
	for _, sw := range swarms {
		if sw.ID == link.DefaultSwarmID {
			return sw, "", nil
		}
	}
	return telegramSwarm{}, telegramReplyDefaultGone, nil
}

// role — роль пользователя в рое словом: administrator у администратора,
// иначе роль его членства, пусто — без роли.
func (c *telegramCommands) role(ctx context.Context, userID, swarmID string) (string, error) {
	user, err := c.server.store.GetUser(ctx, userID)
	if err != nil {
		return "", err
	}
	if user.IsAdmin {
		return "administrator", nil
	}
	role, err := c.server.store.GetSwarmRole(ctx, swarmID, userID)
	if errors.Is(err, store.ErrNotFound) {
		return "", nil
	}
	return role, err
}

// manageRefusal — отказ команды, которой нужно право управлять роем, до
// подтверждения; пусто — право есть. doing называет действие: «Stopping
// swarm "x"». Право — то же правило app, что у маршрутов роя.
func (c *telegramCommands) manageRefusal(ctx context.Context, chat telegramChat, sw telegramSwarm, doing string) (string, error) {
	allowed, err := c.server.app.Can(chat.userID, app.ActionManageSwarm, app.SwarmRes(sw.ID))
	if err != nil || allowed {
		return "", err
	}
	if sw.State != app.StateActive {
		return "Swarm " + sw.label() + " is " + string(sw.State) + ": it cannot be changed until it is active.", nil
	}
	role, err := c.role(ctx, chat.userID, sw.ID)
	if err != nil {
		return "", err
	}
	text := doing + " needs the owner or editor role in it"
	if role != "" {
		text += "; you are a " + role
	}
	return text + ".", nil
}

// ─── Чтение ────────────────────────────────────────────────────────────────

// telegramCount — «1 open request», «2 open requests».
func telegramCount(n int64, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return strconv.FormatInt(n, 10) + " " + one + "s"
}

// pageArg разбирает номер страницы arg команды usage.
func pageArg(arg, usage string) (int, string) {
	page, ok := telegramPageNumber(arg)
	if !ok {
		return 0, "The page must be a number from 1: " + usage + "."
	}
	return page, ""
}

// swarms — /swarms [page]: рои пользователя с исполнением и открытыми
// запросами, сводка — та же выборка, что у списка роёв панели.
func (c *telegramCommands) swarms(ctx context.Context, chat telegramChat, text string) (telegramReply, error) {
	fields, _ := telegramFields(text, 2)
	page, refusal := pageArg(telegramArg(fields, 1), "/swarms [page]")
	if refusal != "" {
		return telegramText(refusal), nil
	}
	swarms, err := c.readable(chat.userID)
	if err != nil {
		return telegramReply{}, err
	}
	if len(swarms) == 0 {
		return telegramText(telegramReplyNoSwarms), nil
	}
	ids := make([]string, 0, len(swarms))
	for _, sw := range swarms {
		ids = append(ids, sw.ID)
	}
	summaries, err := c.server.store.SwarmSummaries(ctx, ids)
	if err != nil {
		return telegramReply{}, err
	}
	var items []string
	for _, sw := range swarms {
		summary, ok := summaries[sw.ID]
		if !ok {
			continue
		}
		item := telegramClipText(sw.Name, telegramNameLimit) + " (" + telegramClipText(sw.project, telegramNameLimit) + "): " + summary.Execution
		if summary.OpenRequests > 0 {
			item += ", " + telegramCount(summary.OpenRequests, "open request")
		}
		if sw.State != app.StateActive {
			item += ", swarm " + string(sw.State)
		}
		items = append(items, item)
	}
	out, refusal := telegramPaged("Your swarms", items, "", "/swarms", page)
	if refusal != "" {
		return telegramText(refusal), nil
	}
	return telegramText(out), nil
}

// swarm — /swarm [swarm]: исполнение, снимок, текущий шаг и задача
// прохода, открытые запросы и managed-акторы роя.
func (c *telegramCommands) swarm(ctx context.Context, chat telegramChat, text string) (telegramReply, error) {
	fields, _ := telegramFields(text, 2)
	sw, refusal, err := c.target(ctx, chat, telegramArg(fields, 1))
	if err != nil || refusal != "" {
		return telegramText(refusal), err
	}
	summaries, err := c.server.store.SwarmSummaries(ctx, []string{sw.ID})
	if err != nil {
		return telegramReply{}, err
	}
	summary, ok := summaries[sw.ID]
	if !ok {
		return telegramText(telegramNotFound(sw.Name)), nil
	}
	steps, err := c.server.store.SwarmSteps(ctx, sw.ID)
	if err != nil {
		return telegramReply{}, err
	}
	role, err := c.role(ctx, chat.userID, sw.ID)
	if err != nil {
		return telegramReply{}, err
	}
	lines := []string{"Swarm " + sw.label() + " (project " + telegramClipText(sw.project, telegramNameLimit) + "), your role: " + role}
	if sw.State != app.StateActive {
		lines = append(lines, "Swarm state: "+string(sw.State))
	}
	lines = append(lines, "Execution: "+summary.Execution)
	if summary.DefinitionName != "" {
		lines = append(lines, "Workflow: "+telegramClipText(summary.DefinitionName, telegramNameLimit)+" v"+strconv.FormatInt(summary.DefinitionVersion, 10))
	}
	if len(steps) == 0 {
		lines = append(lines, "No pass has run yet.")
	} else {
		lines = append(lines, "Current step: "+telegramStep(steps[len(steps)-1]))
		if goal := currentPassGoal(steps); goal != "" {
			lines = append(lines, "Goal: "+telegramClipText(goal, telegramGoalLimit))
		}
	}
	lines = append(lines, "Open requests: "+strconv.FormatInt(summary.OpenRequests, 10), "Managed actors: "+strconv.FormatInt(summary.ManagedActorCount, 10))
	return telegramText(strings.Join(lines, "\n")), nil
}

// telegramStep — шаг для текста: номер, узел, состояние и причина.
func telegramStep(v store.PipelineVisit) string {
	text := strconv.FormatInt(v.Sequence, 10) + ", node " + telegramClipText(v.NodeID, telegramNameLimit) + ", " + v.State
	if v.Detail != "" {
		text += ": " + telegramLine(v.Detail)
	}
	return text
}

// use — /use [swarm]: рой по умолчанию чата в строке привязки; без роя —
// какой он сейчас.
func (c *telegramCommands) use(ctx context.Context, chat telegramChat, text string) (telegramReply, error) {
	fields, _ := telegramFields(text, 2)
	ref := telegramArg(fields, 1)
	if ref == "" {
		sw, refusal, err := c.target(ctx, chat, "")
		if err != nil || refusal != "" {
			return telegramText(refusal), err
		}
		return telegramText("The default swarm of this chat is " + sw.label() + " (project " + telegramClipText(sw.project, telegramNameLimit) + ")."), nil
	}
	sw, refusal, err := c.resolve(chat.userID, ref)
	if err != nil || refusal != "" {
		return telegramText(refusal), err
	}
	c.writing()
	_, err = c.server.store.SetTelegramSwarm(ctx, chat.userID, sw.ID, c.session(chat))
	switch {
	case errors.Is(err, store.ErrNotFound):
		return telegramText(telegramNotFound(ref)), nil
	case errors.Is(err, store.ErrTelegramUnlinked):
		return telegramText(telegramReplyUnlinked), nil
	case err != nil:
		return telegramReply{}, err
	}
	return telegramText("Swarm " + sw.label() + " (project " + telegramClipText(sw.project, telegramNameLimit) + ") is the default swarm of this chat now."), nil
}

// requests — /requests [page]: открытые запросы роя по умолчанию в виде,
// который отдаёт список запросов панели.
func (c *telegramCommands) requests(ctx context.Context, chat telegramChat, text string) (telegramReply, error) {
	fields, _ := telegramFields(text, 2)
	page, refusal := pageArg(telegramArg(fields, 1), "/requests [page]")
	if refusal != "" {
		return telegramText(refusal), nil
	}
	sw, refusal, err := c.target(ctx, chat, "")
	if err != nil || refusal != "" {
		return telegramText(refusal), err
	}
	all, err := c.server.store.SwarmRequests(ctx, sw.ID)
	if err != nil {
		return telegramReply{}, err
	}
	var open []store.SwarmRequest
	for _, req := range all {
		if req.State == "open" {
			open = append(open, req)
		}
	}
	if len(open) == 0 {
		return telegramText("Swarm " + sw.label() + " has no open requests."), nil
	}
	views, err := c.server.swarmRequestViews(ctx, sw.ID, open)
	if err != nil {
		return telegramReply{}, err
	}
	var items []string
	for _, v := range views {
		var asker string
		switch {
		case v.Origin == "node":
			asker = "node " + v.NodeID
			if v.Sequence != nil {
				asker += ", step " + strconv.FormatInt(*v.Sequence, 10)
			}
		case v.Actor != nil && v.Actor.Name != "":
			asker = "actor " + v.Actor.Name
		default:
			asker = "an actor"
		}
		items = append(items, asker+" ("+v.Kind+"): "+v.Question)
	}
	footer := ""
	if c.panel != "" {
		footer = "Answer them in brood: " + telegramSwarmLink(c.panel, sw.ID)
	}
	out, refusal := telegramPaged("Open requests of swarm "+sw.label(), items, footer, "/requests", page)
	if refusal != "" {
		return telegramText(refusal), nil
	}
	return telegramText(out), nil
}

// telegramSwarmLink — страница роя в панели (hash router, как у
// уведомлений).
func telegramSwarmLink(panel, swarmID string) string {
	return panel + "/#/swarms/" + url.PathEscape(swarmID)
}

// actors — /actors [swarm] [page]: managed-акторы роя командой каталога
// agents.
func (c *telegramCommands) actors(ctx context.Context, chat telegramChat, text string) (telegramReply, error) {
	fields, _ := telegramFields(text, 3)
	ref := telegramArg(fields, 1)
	page, refusal := pageArg(telegramArg(fields, 2), "/actors [swarm] [page]")
	if refusal != "" {
		return telegramText(refusal), nil
	}
	sw, refusal, err := c.target(ctx, chat, ref)
	if err != nil || refusal != "" {
		return telegramText(refusal), err
	}
	inv, err := command.ParseTool("agents", json.RawMessage(`{}`))
	if err != nil {
		return telegramReply{}, err
	}
	result, link, err := c.catalog(ctx, chat, sw.ID, inv)
	switch {
	case link == telegramLinkEnded:
		return telegramReply{}, nil
	case link == telegramLinkUnchecked:
		return telegramText(telegramReplyFailed), nil
	case err != nil:
		return telegramText(c.catalogRefusal("list the actors of swarm "+sw.label(), "Listing the actors of swarm "+sw.label(), err)), nil
	}
	var data struct {
		Actors []struct {
			Name       string `json:"name"`
			Role       string `json:"role"`
			State      string `json:"state"`
			Generation int64  `json:"generation"`
		} `json:"actors"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		return telegramReply{}, fmt.Errorf("decode the agents result: %w", err)
	}
	if len(data.Actors) == 0 {
		return telegramText("Swarm " + sw.label() + " has no managed actors."), nil
	}
	var items []string
	for _, a := range data.Actors {
		item := telegramClipText(a.Name, telegramNameLimit)
		if a.Role != "" {
			item += " (role " + telegramClipText(a.Role, telegramNameLimit) + ")"
		}
		items = append(items, item+": "+a.State+", generation "+strconv.FormatInt(a.Generation, 10))
	}
	next := "/actors " + sw.ID
	if ref != "" {
		next = "/actors " + ref
	}
	out, refusal := telegramPaged("Managed actors of swarm "+sw.label(), items, "", next, page)
	if refusal != "" {
		return telegramText(refusal), nil
	}
	return telegramText(out), nil
}

// ─── Изменения после подтверждения ─────────────────────────────────────────

// confirmation — ответ с кнопками подтверждения действия run: button
// подтверждает, Cancel отменяет.
func (c *telegramCommands) confirmation(chat telegramChat, question, button string, run func(context.Context) string) (telegramReply, error) {
	id, err := c.confirm.add(chat, run)
	if err != nil {
		return telegramReply{}, err
	}
	return telegramReply{
		text: question + "\n\nConfirm within " + telegramMinutes(telegramConfirmTTL) + ".",
		markup: &telegram.InlineKeyboardMarkup{InlineKeyboard: [][]telegram.InlineKeyboardButton{{
			{Text: button, CallbackData: telegramConfirmData(id, true)},
			{Text: "Cancel", CallbackData: telegramConfirmData(id, false)},
		}}},
	}, nil
}

// stop — /stop <swarm>: остановка прохода после подтверждения.
// Подтверждение держит проход, который шёл при нём: нажатие после чужой
// остановки и нового прохода новый проход не останавливает.
func (c *telegramCommands) stop(ctx context.Context, chat telegramChat, text string) (telegramReply, error) {
	fields, _ := telegramFields(text, 2)
	ref := telegramArg(fields, 1)
	if ref == "" {
		return telegramText("Name a swarm: /stop <swarm>."), nil
	}
	sw, refusal, err := c.resolve(chat.userID, ref)
	if err != nil || refusal != "" {
		return telegramText(refusal), err
	}
	doing := "Stopping swarm " + sw.label()
	if refusal, err := c.manageRefusal(ctx, chat, sw, doing); err != nil || refusal != "" {
		return telegramText(refusal), err
	}
	steps, err := c.server.store.SwarmSteps(ctx, sw.ID)
	if err != nil {
		return telegramReply{}, err
	}
	pass := currentPass(steps)
	question := "Stop the running pass of swarm " + sw.label() + " (project " + telegramClipText(sw.project, telegramNameLimit) + ")? Its open request is cancelled; actors, workspaces and role bindings stay."
	return c.confirmation(chat, question, "Stop pass", func(ctx context.Context) string {
		verb := "stop swarm " + sw.label()
		if refusal, err := c.passChanged(ctx, chat, sw, pass); err != nil {
			slog.Error("telegram command failed", "command", verb, "err", err.Error())
			return telegramReplyNotRun
		} else if refusal != "" {
			return refusal
		}
		c.writing()
		stopped, err := c.server.stopSwarm(ctx, sw.ID, chat.userID, c.session(chat))
		if err != nil {
			return c.executionRefusal(ctx, chat, verb, doing, err)
		}
		return "Swarm " + sw.label() + " stopped: " + c.executionState(ctx, stopped)
	})
}

// currentPass — проход, который идёт или шёл последним: проход последнего
// шага, 0 — шагов нет.
func currentPass(steps []store.PipelineVisit) int64 {
	if len(steps) == 0 {
		return 0
	}
	return steps[len(steps)-1].Pass
}

// passChanged — отказ нажатия /stop, когда рой после подтверждения начал
// другой проход: подтверждали остановку прохода pass. Пусто — проход тот
// же или рой больше не виден пользователю (его скроет stopSwarm).
func (c *telegramCommands) passChanged(ctx context.Context, chat telegramChat, sw telegramSwarm, pass int64) (string, error) {
	if visible, err := c.visible(chat, sw); err != nil || !visible {
		return "", err
	}
	steps, err := c.server.store.SwarmSteps(ctx, sw.ID)
	if err != nil || currentPass(steps) == pass {
		return "", err
	}
	return "Swarm " + sw.label() + " started another pass since you confirmed; nothing was done. Send /stop again to stop the pass that runs now.", nil
}

// start — /start <swarm> <goal>: новый проход на закреплённом определении
// роя после подтверждения. /start без роя или с одним словом, которое не
// называет рой, — напоминание о привязке, как до #284: так чат отвечает на
// повтор ссылки t.me с кодом.
func (c *telegramCommands) start(ctx context.Context, chat telegramChat, text string) (telegramReply, error) {
	fields, goal := telegramFields(text, 2)
	ref := telegramArg(fields, 1)
	if ref == "" {
		return telegramText(telegramReplyAlreadyLinked), nil
	}
	sw, refusal, err := c.resolve(chat.userID, ref)
	if err != nil {
		return telegramReply{}, err
	}
	if refusal != "" {
		if goal == "" {
			return telegramText(telegramReplyAlreadyLinked), nil
		}
		return telegramText(refusal), nil
	}
	if goal == "" {
		return telegramText("Name the goal of the pass: /start <swarm> <goal>."), nil
	}
	doing := "Starting swarm " + sw.label()
	if refusal, err := c.manageRefusal(ctx, chat, sw, doing); err != nil || refusal != "" {
		return telegramText(refusal), err
	}
	pinned, err := c.server.store.GetSwarm(ctx, sw.ID)
	if err != nil {
		return telegramReply{}, err
	}
	if pinned.Graph == nil || pinned.DefinitionName == "" {
		return telegramText("Swarm " + sw.label() + " has not run a pass yet: start its first pass in brood, where you choose its workflow."), nil
	}
	if err := startGoalError(goal); err != nil {
		return telegramText("This goal cannot start a pass: " + err.Error() + "."), nil
	}
	definition := pinned.DefinitionName
	question := "Start a new pass of swarm " + sw.label() + " (project " + telegramClipText(sw.project, telegramNameLimit) + ") on workflow " +
		telegramClipText(definition, telegramNameLimit) + ", at the current HEAD of the project repository?\n\nGoal:\n" + telegramClipText(goal, telegramGoalLimit)
	return c.confirmation(chat, question, "Start pass", func(ctx context.Context) string {
		in := store.SwarmStart{SwarmID: sw.ID, UserID: chat.userID, Definition: definition, Goal: goal}
		c.writing()
		started, err := c.server.startSwarmPass(ctx, in, c.session(chat))
		if err != nil {
			return c.executionRefusal(ctx, chat, "start swarm "+sw.label(), doing, err)
		}
		// Первый шаг ждёт диспетчера после коммита, как после запуска из
		// панели.
		defer c.server.steps.kick(started.ID)
		return "Swarm " + sw.label() + " started a new pass on workflow " + telegramClipText(definition, telegramNameLimit) + ": " + c.executionState(ctx, started)
	})
}

// executionState — исполнение роя после запуска или остановки и его
// последний шаг.
func (c *telegramCommands) executionState(ctx context.Context, sw store.Swarm) string {
	text := "execution " + sw.Execution
	steps, err := c.server.store.SwarmSteps(ctx, sw.ID)
	if err != nil || len(steps) == 0 {
		return text + "."
	}
	last := steps[len(steps)-1]
	return text + ", last step " + strconv.FormatInt(last.Sequence, 10) + " at node " + telegramClipText(last.NodeID, telegramNameLimit) + " (" + last.State + ")."
}

// executionRefusal — отказ запуска или остановки словами: публичная
// причина (swarmExecutionRefusal), отказ в праве — нужная роль. Сессия
// чата, которая умерла до записи, ответа не получает: чат больше не
// привязан; привязку, которую не удалось проверить, отказ не судит.
func (c *telegramCommands) executionRefusal(ctx context.Context, chat telegramChat, verb, doing string, err error) string {
	status, reason, ok := swarmExecutionRefusal(err)
	switch {
	case !ok:
		slog.Error("telegram command failed", "command", verb, "err", err.Error())
		return "brood could not " + verb + " right now; nothing is known to have changed. Try again later."
	case status == http.StatusForbidden:
		live, err := c.stillLinked(ctx, chat)
		if err != nil {
			slog.Error("telegram command failed", "command", verb, "err", err.Error())
			return "brood refused to " + verb + "; nothing was done. Try again later."
		}
		if !live {
			return ""
		}
		return doing + " needs the owner or editor role in it."
	case status == http.StatusNotFound && errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrWorkflowDefinitionMissing):
		return "The swarm is no longer among your swarms; nothing was done."
	}
	return "brood refused to " + verb + ": " + reason
}

// actor — /actor <swarm> <name> stop|start|update|reset: команда каталога
// того же имени после подтверждения. Подтверждение держит актора и его
// поколение: нажатие после чужого reset или пересоздания актора того же
// имени ничего не делает.
func (c *telegramCommands) actor(ctx context.Context, chat telegramChat, text string) (telegramReply, error) {
	fields, _ := telegramFields(text, 4)
	if len(fields) < 4 {
		return telegramText("Name a swarm, an actor and an action: /actor <swarm> <name> stop|start|update|reset."), nil
	}
	ref, name, action := fields[1], fields[2], strings.ToLower(fields[3])
	if !slices.Contains(telegramActorActions, action) {
		return telegramText("Unknown actor action " + telegramQuote(fields[3]) + ": use stop, start, update or reset."), nil
	}
	sw, refusal, err := c.resolve(chat.userID, ref)
	if err != nil || refusal != "" {
		return telegramText(refusal), err
	}
	actor := telegramQuote(name)
	doing := "Changing actor " + actor + " in swarm " + sw.label()
	if refusal, err := c.manageRefusal(ctx, chat, sw, doing); err != nil || refusal != "" {
		return telegramText(refusal), err
	}
	pinned, err := c.server.store.LookupManagedActor(ctx, sw.ID, name)
	if errors.Is(err, store.ErrNotFound) {
		return telegramText("No managed actor " + actor + " in swarm " + sw.label() + ". Send /actors " + telegramClipText(ref, telegramNameLimit) + " for the list."), nil
	} else if err != nil {
		return telegramReply{}, err
	}
	args := map[string]any{"name": name}
	if action == "reset" {
		args["confirm"] = true
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return telegramReply{}, err
	}
	verb := action + " actor " + actor + " in swarm " + sw.label()
	inv, err := command.ParseTool(action, raw)
	if err != nil {
		return telegramText(c.catalogRefusal(verb, doing, err)), nil
	}
	question, button := telegramActorQuestion(action, actor, sw.label())
	return c.confirmation(chat, question, button, func(ctx context.Context) string {
		if refusal, err := c.actorChanged(ctx, chat, sw, pinned, actor); err != nil {
			slog.Error("telegram command failed", "command", verb, "err", err.Error())
			return telegramReplyNotRun
		} else if refusal != "" {
			return refusal
		}
		c.writing()
		result, link, err := c.catalog(ctx, chat, sw.ID, inv)
		the := "the " + action + " of actor " + actor + " in swarm " + sw.label()
		switch {
		case link == telegramLinkEnded:
			return "This chat is no longer linked to your brood account: " + the + " may have taken effect. Check the actor in brood."
		case link == telegramLinkUnchecked || unknownOutcome(result):
			return "The outcome of " + the + " is unknown: it may have taken effect. Check the actor in brood before you try again."
		case err != nil:
			return c.catalogRefusal(verb, doing, err)
		}
		what := strings.ToUpper(action[:1]) + action[1:] + " of actor " + actor + " in swarm " + sw.label()
		if result.Outcome == "accepted" {
			return what + " was accepted: " + telegramClipText(result.Text, telegramGoalLimit)
		}
		return what + " is done: " + telegramClipText(result.Text, telegramGoalLimit)
	})
}

// actorChanged — отказ нажатия /actor, когда актора pinned с подтверждения
// сбросили (новое поколение), пересоздали или удалили. Пусто — актор тот же
// или рой больше не виден пользователю (его скроет команда каталога).
func (c *telegramCommands) actorChanged(ctx context.Context, chat telegramChat, sw telegramSwarm, pinned store.ManagedActor, actor string) (string, error) {
	if visible, err := c.visible(chat, sw); err != nil || !visible {
		return "", err
	}
	current, err := c.server.store.LookupManagedActor(ctx, sw.ID, pinned.Name)
	if errors.Is(err, store.ErrNotFound) {
		return "No managed actor " + actor + " in swarm " + sw.label() + " any more; nothing was done.", nil
	}
	if err != nil || (current.UID == pinned.UID && current.Incarnation == pinned.Incarnation) {
		return "", err
	}
	return "Actor " + actor + " in swarm " + sw.label() + " was reset or replaced since you confirmed; nothing was done. Send the command again to act on the actor as it is now.", nil
}

// telegramActorQuestion — вопрос подтверждения действия над актором и
// подпись кнопки.
func telegramActorQuestion(action, actor, swarm string) (string, string) {
	in := " actor " + actor + " in swarm " + swarm
	switch action {
	case "stop":
		return "Stop" + in + "? Its container stops; the workspace is kept.", "Stop actor"
	case "start":
		return "Start" + in + " in its retained container and workspace?", "Start actor"
	case "update":
		return "Update" + in + " to the server's current guest release? It restarts in its container and keeps its workspace.", "Update actor"
	}
	return "Reset" + in + "? Its container and workspace are destroyed and a new generation starts; this cannot be undone.", "Reset actor"
}

// unknownOutcome: команда каталога могла исполниться, а подтверждение
// потерялось; повтор без проверки может повторить эффект.
func unknownOutcome(result command.Result) bool {
	return result.Outcome == "unconfirmed" || result.Outcome == "unknown"
}

// telegramLink — привязка чата после команды каталога.
type telegramLink int

const (
	// telegramLinkKept — привязка жива или команда не исполнялась.
	telegramLinkKept telegramLink = iota
	// telegramLinkEnded — привязка умерла, пока шла команда: итог чату не
	// принадлежит.
	telegramLinkEnded
	// telegramLinkUnchecked — проверить привязку после команды не удалось:
	// итог чату не показывается.
	telegramLinkUnchecked
)

// catalog исполняет команду каталога inv в рое swarmID от имени пользователя
// чата тем же путём, что POST /api/commands/{tool}: проверки маршрута
// (commandPreflight) и исполнитель каталога; полномочие сессии команды —
// сессия чата (его читают команды сессии гостя, запись жизненного цикла
// его не читает). После исполненной команды привязка проверяется снова, как
// веб-сессия (validProxySession): итог и ошибка возвращаются вместе с ней, и
// вызывающий не показывает итог чату, чья привязка умерла или не проверена.
func (c *telegramCommands) catalog(ctx context.Context, chat telegramChat, swarmID string, inv command.Invocation) (command.Result, telegramLink, error) {
	s := c.server
	user, err := s.store.GetUser(ctx, chat.userID)
	if err != nil {
		return command.Result{}, telegramLinkKept, err
	}
	refusal, failure := s.commandPreflight(ctx, user, swarmID, inv)
	if failure != nil {
		return command.Result{}, telegramLinkKept, failure
	}
	if refusal != nil {
		return command.Result{}, telegramLinkKept, refusal
	}
	commandContext := context.WithValue(ctx, commandSessionAuthorityKey{}, c.session(chat))
	result, err := s.executeOperatorCommand(commandContext, user.ID, swarmID, "user:"+user.ID+":telegram", inv)
	live, checkErr := c.stillLinked(ctx, chat)
	switch {
	case checkErr != nil:
		slog.Error("telegram command failed", "command", inv.Name, "err", checkErr.Error())
		return result, telegramLinkUnchecked, err
	case !live:
		return result, telegramLinkEnded, err
	}
	return result, telegramLinkKept, err
}

// catalogRefusal — отказ команды каталога словами: публичная причина
// (#276), отказ в праве — нужная роль; прочее — общий отказ без текста.
func (c *telegramCommands) catalogRefusal(verb, doing string, err error) string {
	if reason := command.PublicReason(err); reason != "" {
		return "brood refused to " + verb + ": " + reason
	}
	if errors.Is(err, app.ErrNotAllowed) {
		return doing + " needs the owner or editor role in it."
	}
	slog.Error("telegram command failed", "command", verb, "err", err.Error())
	return "brood could not " + verb + " right now; nothing is known to have changed. Try again later."
}

// ─── Разбор и страницы ─────────────────────────────────────────────────────

// telegramFields — первые n слов text и текст после них как есть, без
// пробелов по краям: задача /start сохраняет переносы строк.
func telegramFields(text string, n int) ([]string, string) {
	var fields []string
	rest := strings.TrimLeftFunc(text, unicode.IsSpace)
	for len(fields) < n && rest != "" {
		end := strings.IndexFunc(rest, unicode.IsSpace)
		if end < 0 {
			fields = append(fields, rest)
			rest = ""
			break
		}
		fields = append(fields, rest[:end])
		rest = strings.TrimLeftFunc(rest[end:], unicode.IsSpace)
	}
	return fields, strings.TrimRightFunc(rest, unicode.IsSpace)
}

// telegramArg — слово i команды, пусто — его нет.
func telegramArg(fields []string, i int) string {
	if i < len(fields) {
		return fields[i]
	}
	return ""
}

// telegramPageNumber — номер страницы из слова arg, без него — первая.
func telegramPageNumber(arg string) (int, bool) {
	if arg == "" {
		return 1, true
	}
	page, err := strconv.Atoi(arg)
	if err != nil || page < 1 {
		return 0, false
	}
	return page, true
}

// telegramLine — одна строка списка: без переносов и не длиннее
// telegramLineLimit.
func telegramLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(s, "?"))
	return telegramClipText(s, telegramLineLimit)
}

// telegramPaged — страница page списка items: заголовок с номером и числом
// страниц, по строке на элемент, ссылка на следующую страницу (command <n>)
// и footer. Страница вне списка — отказ с числом страниц.
func telegramPaged(title string, items []string, footer, command string, page int) (string, string) {
	pages := max(1, (len(items)+telegramPageSize-1)/telegramPageSize)
	if page > pages {
		unit := "pages"
		if pages == 1 {
			unit = "page"
		}
		return "", fmt.Sprintf("There is no page %d: the list has %d %s.", page, pages, unit)
	}
	head := fmt.Sprintf(", page %d of %d:", page, pages)
	lines := []string{telegramLine(telegramClipText(title, telegramLineLimit-telegram.TextLength(head)) + head)}
	for _, item := range items[(page-1)*telegramPageSize : min(page*telegramPageSize, len(items))] {
		lines = append(lines, telegramLine("- "+item))
	}
	if page < pages {
		lines = append(lines, telegramLine(fmt.Sprintf("Next page: %s %d", command, page+1)))
	}
	if footer != "" {
		lines = append(lines, telegramLine(footer))
	}
	return telegramClipText(strings.Join(lines, "\n"), telegram.MessageLimit), ""
}

package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/command"
	"brood.dev/go/brood/proxmox/relay"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

// telegramOperator — имя адресата шины, за которым стоит владелец роя.
const telegramOperator = "operator"

const (
	telegramRouteLimit      = 256
	telegramChoicesPage     = 8
	telegramUseCallback     = "use:"
	telegramHelp            = "Write to the selected agent, or reply to an agent's message to answer that agent. /use opens the agent picker; /use PROJECT/SWARM/AGENT selects one. /agents also opens the picker; /swarms lists your active swarms."
	telegramChooseAgent     = "You need to choose an agent. The message was not sent."
	telegramListUnavailable = "The agent list is unavailable. Try /use again later."
	telegramStaleChoice     = "This button is no longer valid. Open /use again."
)

// telegramChat — привязанный чат обновления: пользователь, бот, чат и
// сессия привязки. Без сессии чат не привязан.
type telegramChat struct {
	userID, botUsername, session string
	chatID                       int64
}

func (c telegramChat) linked() bool { return c.session != "" }

func (c telegramChat) active(ctx context.Context, st *store.Store) bool {
	session, err := st.TelegramChatSession(ctx, c.userID, c.botUsername, c.chatID, c.chatID)
	return err == nil && c.linked() && session == c.session
}

// telegramResponse содержит текст и необязательные строки кнопок выбора.
type telegramResponse struct {
	text    string
	choices [][]telegramChoice
}

type telegramChoice struct{ label, action string }

// telegramClipText обрезает текст до предела сообщения Bot API.
func telegramClipText(text string, limit int) string {
	if telegram.TextLength(text) <= limit {
		return text
	}
	return telegram.SplitText(text)[0]
}

// telegramRoute хранит устойчивый адрес отправителя в рамках привязки.
type telegramRoute struct{ session, swarmID, actorUID string }

type telegramChannel struct {
	server *Server
	bots   *telegramBots

	mu     sync.Mutex
	routes map[int64]map[int64]telegramRoute
	order  map[int64][]int64
}

func newTelegramChannel(s *Server, bots *telegramBots) *telegramChannel {
	return &telegramChannel{server: s, bots: bots, routes: map[int64]map[int64]telegramRoute{}, order: map[int64][]int64{}}
}

func (c *telegramChannel) remember(chatID, messageID int64, route telegramRoute) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.routes[chatID] == nil {
		c.routes[chatID] = map[int64]telegramRoute{}
	}
	if _, exists := c.routes[chatID][messageID]; !exists {
		c.order[chatID] = append(c.order[chatID], messageID)
	}
	c.routes[chatID][messageID] = route
	if len(c.order[chatID]) > telegramRouteLimit {
		delete(c.routes[chatID], c.order[chatID][0])
		c.order[chatID] = c.order[chatID][1:]
	}
}

func (c *telegramChannel) route(chat telegramChat, messageID int64) (telegramRoute, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	route, ok := c.routes[chat.chatID][messageID]
	return route, ok && route.session == chat.session
}

type telegramSwarm struct {
	store.Swarm
	path string
}

type telegramRecipient struct {
	store.Agent
	path string
}

func telegramCanMessage(user store.User, sw store.Swarm) bool {
	return !user.Disabled && sw.State == "active" && (user.IsAdmin || sw.OwnerID == user.ID)
}

func (c *telegramChannel) swarms(ctx context.Context, user store.User) ([]telegramSwarm, error) {
	projects, err := c.server.store.ListProjects(ctx)
	if err != nil {
		return nil, err
	}
	var out []telegramSwarm
	for _, project := range projects {
		rows, err := c.server.store.ListSwarmsByProject(ctx, project.ID)
		if err != nil {
			return nil, err
		}
		for _, sw := range rows {
			if telegramCanMessage(user, sw) {
				out = append(out, telegramSwarm{sw, project.Name + "/" + sw.Name})
			}
		}
	}
	return out, nil
}

func (c *telegramChannel) available(a store.Agent) bool {
	m := c.server.provider
	return a.Phase == "running" && a.Desired == "running" && m != nil && m.hub != nil && m.deliverMessage != nil && m.hub.Connected(a.UID, a.Incarnation)
}

func (c *telegramChannel) recipients(ctx context.Context, swarms []telegramSwarm) ([]telegramRecipient, error) {
	var out []telegramRecipient
	for _, sw := range swarms {
		rows, err := c.server.store.ListAgents(ctx, sw.ID)
		if err != nil {
			return nil, err
		}
		for _, a := range rows {
			if c.available(a) {
				out = append(out, telegramRecipient{a, sw.path + "/" + a.Name})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].path < out[j].path })
	return out, nil
}

// receive возвращает пустой ответ только при успешной доставке или когда
// нечего обработать. Текст сообщения не разбирается на имя и задание.
func (c *telegramChannel) receive(ctx context.Context, chat telegramChat, m *telegram.Message) telegramResponse {
	if strings.TrimSpace(m.Text) == "" {
		return telegramResponse{}
	}
	if name, arg, ok := telegramCommand(m.Text); ok {
		switch name {
		case "start", "help", "swarms", "use", "agents":
			return c.command(ctx, chat, name, arg)
		}
		forward := *m
		forward.Text = "/" + name
		if arg != "" {
			forward.Text += " " + arg
		}
		m = &forward
	}
	user, err := c.server.store.GetUser(ctx, chat.userID)
	if err != nil || user.Disabled || !chat.active(ctx, c.server.store) {
		return telegramResponse{}
	}
	if m.ReplyToMessage != nil {
		return c.receiveReply(ctx, chat, user, m)
	}
	link, err := c.server.store.GetTelegramLink(ctx, chat.userID)
	if err != nil {
		return telegramResponse{text: "The selected agent could not be checked. The message was not sent."}
	}
	swarms, err := c.swarms(ctx, user)
	if err != nil {
		return telegramResponse{text: "The swarm list is unavailable. The message was not sent."}
	}
	recipients, err := c.recipients(ctx, swarms)
	if err != nil {
		return telegramResponse{text: "The agent list is unavailable. The message was not sent."}
	}
	var target telegramRecipient
	if link.DefaultActorUID != "" {
		for _, a := range recipients {
			if a.UID == link.DefaultActorUID {
				target = a
				break
			}
		}
	} else if len(swarms) == 1 && len(recipients) > 0 {
		master, err := c.server.store.BuiltInMasterRoleID(ctx)
		if err != nil {
			return telegramResponse{text: "The master's role could not be checked. The message was not sent."}
		}
		for _, a := range recipients {
			if master == "" || a.Role != master {
				continue
			}
			if target.UID != "" {
				target = telegramRecipient{}
				break
			}
			target = a
		}
	}
	if target.UID == "" {
		return telegramAgentChoices(chat, recipients, link.DefaultActorUID, telegramChooseAgent, 0)
	}
	response, unavailable := c.deliver(ctx, chat, user, target, m.Text)
	if unavailable {
		return c.pick(ctx, chat, telegramChooseAgent, 0)
	}
	return response
}

func (c *telegramChannel) receiveReply(ctx context.Context, chat telegramChat, user store.User, m *telegram.Message) telegramResponse {
	reply := m.ReplyToMessage
	artifact, err := c.server.store.GetTelegramArtifactReply(ctx, chat.session, reply.MessageID)
	var swarmID, actorUID string
	text := m.Text
	switch {
	case err == nil:
		swarmID, actorUID = artifact.SwarmID, artifact.ActorUID
		if _, _, slash := telegramCommand(text); !slash {
			text += "\n\n[brood] This operator reply is about artifact version " + c.server.artifactURL(swarmID, artifact.VersionID)
		}
	case !errors.Is(err, store.ErrNotFound) || strings.Contains(reply.Text, artifactNoticeMarker):
		return telegramResponse{text: "The version for this reply could not be confirmed. Nothing was sent. Ask the agent to resend the artifact notice."}
	default:
		route, ok := c.route(chat, reply.MessageID)
		if !ok {
			return telegramResponse{text: "The original recipient could not be confirmed. Nothing was sent. Ask the agent to send a new message."}
		}
		swarmID, actorUID = route.swarmID, route.actorUID
	}
	a, actorErr := c.server.store.GetAgent(ctx, actorUID)
	sw, swarmErr := c.server.store.GetSwarm(ctx, swarmID)
	if actorErr != nil || swarmErr != nil || a.SwarmID != sw.ID || !telegramCanMessage(user, sw) || !c.available(a) {
		return telegramResponse{text: "The original recipient of this reply is unavailable. Nothing was sent."}
	}
	project, err := c.server.store.GetProject(ctx, sw.ProjectID)
	if err != nil {
		return telegramResponse{text: "The original recipient could not be confirmed. Nothing was sent."}
	}
	response, _ := c.deliver(ctx, chat, user, telegramRecipient{a, project.Name + "/" + sw.Name + "/" + a.Name}, text)
	return response
}

// deliver повторно проверяет привязку, права и адрес по UID на границе
// отправки. Никогда не повторяет отправку при неизвестном исходе.
func (c *telegramChannel) deliver(ctx context.Context, chat telegramChat, user store.User, target telegramRecipient, text string) (telegramResponse, bool) {
	st := c.server.store
	resolve := func(ctx context.Context) (store.Agent, error) {
		return st.GetAgent(ctx, target.UID)
	}
	authorize := func(ctx context.Context) error {
		if !chat.active(ctx, st) {
			return app.ErrNotAllowed
		}
		current, err := st.GetUser(ctx, chat.userID)
		sw, swarmErr := st.GetSwarm(ctx, target.SwarmID)
		if err != nil || swarmErr != nil || !telegramCanMessage(current, sw) {
			return app.ErrNotAllowed
		}
		a, err := resolve(ctx)
		if err != nil || a.SwarmID != sw.ID || a.Incarnation != target.Incarnation || !c.available(a) {
			return errRecipientOffline
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, command.CallTimeout(0))
	defer cancel()
	if _, _, slash := telegramCommand(text); slash {
		if err := authorize(ctx); err != nil {
			return telegramResponse{text: fmt.Sprintf("%s is unavailable. Nothing was sent.", target.path)}, true
		}
		return c.sendSlash(ctx, chat, target, text), false
	}
	msg := relay.MessageRequest{From: "operator:" + user.Username, Body: operatorMessageBody(text)}
	undoFocus := c.server.focusTelegramSubmission(ctx, chat, target.UID, target.Incarnation)
	result, sent, err := c.server.deliverToActor(ctx, target.SwarmID, resolve, msg, authorize)
	if !sent {
		undoFocus()
	}
	switch {
	case err == nil && result.Outcome == "completed":
		return telegramResponse{}, false
	case sent:
		return telegramResponse{text: fmt.Sprintf("Delivery to %s is unconfirmed. Check before sending again.", target.path)}, false
	case errors.Is(err, errRecipientOffline), errors.Is(err, app.ErrNotFound), errors.Is(err, app.ErrNotAllowed):
		return telegramResponse{text: fmt.Sprintf("%s is unavailable. Nothing was sent.", target.path)}, true
	case errors.Is(err, errMessageNotSent):
		return telegramResponse{text: fmt.Sprintf("%s did not take the message. Nothing was sent.", target.path)}, false
	default:
		return telegramResponse{text: "The message was not sent. Try again later."}, false
	}
}

func (c *telegramChannel) sendSlash(ctx context.Context, chat telegramChat, target telegramRecipient, text string) telegramResponse {
	if strings.EqualFold(text, "/escape") {
		text = "Escape"
	}
	args, _ := json.Marshal(map[string]string{"name": target.Name, "command": text})
	ctx = context.WithValue(ctx, commandSessionAuthorityKey{}, func(ctx context.Context) bool { return chat.active(ctx, c.server.store) })
	result, err := c.server.executeOperatorSessionTarget(ctx, chat.userID, "telegram:"+strconv.FormatInt(chat.chatID, 10), command.Invocation{Name: "slash", Arguments: args}, target.Agent, true)
	if err == nil {
		if result.Outcome == "observed" {
			return telegramResponse{text: fmt.Sprintf("%s · terminal output:\n%s", target.path, result.Text)}
		}
		if text == "Escape" {
			return telegramResponse{text: fmt.Sprintf("Interrupt sent to %s. Check the terminal if it is still working.", target.path)}
		}
		return telegramResponse{text: fmt.Sprintf("Command sent to %s, but no result was confirmed. Check the terminal before repeating it.", target.path)}
	}
	if result.Outcome == "unconfirmed" || result.Outcome == "unknown" {
		return telegramResponse{text: fmt.Sprintf("Delivery to %s is unconfirmed. Check before sending again.", target.path)}
	}
	if reason := command.PublicReason(err); reason != "" {
		return telegramResponse{text: reason}
	}
	if result.Outcome == "failed" {
		switch err.Error() {
		case "close the active terminal writer before changing the session", "PTY writer conflict":
			return telegramResponse{text: fmt.Sprintf("The command was not sent to %s: its writable terminal is open. Close the terminal or switch to read-only view, then retry.", target.path)}
		case "invalid slash arguments", "slash command is too long", "slash command must start with / or be Escape":
			return telegramResponse{text: "The command was not sent. Enter a single slash command of at most 255 bytes."}
		case "no proven live Claude session and PTY holder", "session connection is closed", "trusted PTY control unavailable; nothing changed":
			return telegramResponse{text: fmt.Sprintf("The command was not sent to %s: its Claude Code session could not be reached. Check the agent and retry.", target.path)}
		}
	}
	return telegramResponse{text: "The command was not sent. Check the agent and try again."}
}

func (c *telegramChannel) command(ctx context.Context, chat telegramChat, name, arg string) telegramResponse {
	if !chat.active(ctx, c.server.store) {
		return telegramResponse{}
	}
	switch name {
	case "start", "help":
		return telegramResponse{text: telegramHelp}
	case "swarms":
		user, err := c.server.store.GetUser(ctx, chat.userID)
		if err != nil {
			return telegramResponse{text: "The swarm list is unavailable. Try again later."}
		}
		swarms, err := c.swarms(ctx, user)
		if err != nil {
			return telegramResponse{text: "The swarm list is unavailable. Try again later."}
		}
		if len(swarms) == 0 {
			return telegramResponse{text: "You have no active swarm."}
		}
		paths := make([]string, 0, len(swarms))
		for _, sw := range swarms {
			paths = append(paths, sw.path)
		}
		sort.Strings(paths)
		return telegramResponse{text: "Swarms: " + strings.Join(paths, ", ")}
	case "use":
		if arg != "" {
			return c.selectRecipient(ctx, chat, func(a telegramRecipient) bool { return a.path == arg })
		}
		return c.pick(ctx, chat, "Choose an agent for your next messages.", 0)
	case "agents":
		return c.pick(ctx, chat, "Choose an agent for your next messages.", 0)
	}
	return telegramResponse{text: "Unknown command. " + telegramHelp}
}

func (c *telegramChannel) recipientList(ctx context.Context, chat telegramChat) ([]telegramRecipient, error) {
	user, err := c.server.store.GetUser(ctx, chat.userID)
	if err != nil {
		return nil, err
	}
	swarms, err := c.swarms(ctx, user)
	if err != nil {
		return nil, err
	}
	return c.recipients(ctx, swarms)
}

func (c *telegramChannel) selectRecipient(ctx context.Context, chat telegramChat, match func(telegramRecipient) bool) telegramResponse {
	st := c.server.store
	if !chat.active(ctx, st) {
		return telegramResponse{text: telegramStaleChoice}
	}
	available, err := c.recipientList(ctx, chat)
	if err != nil {
		return telegramResponse{text: "The agent list is unavailable. Your selection did not change. Try /use again later."}
	}
	for _, a := range available {
		if !match(a) || !c.available(a.Agent) {
			continue
		}
		err := st.SetTelegramActor(ctx, chat.userID, chat.session, a.UID)
		switch {
		case err == nil:
			c.server.clearTelegramFocus(chat)
			return telegramResponse{text: "Messages now go to " + a.path + "."}
		case errors.Is(err, store.ErrNotFound):
			return c.pick(ctx, chat, "That agent is unavailable. Your selection did not change.", 0)
		case errors.Is(err, store.ErrTelegramUnlinked):
			return telegramResponse{text: telegramStaleChoice}
		default:
			return telegramResponse{text: "The agent could not be selected. Try /use again later."}
		}
	}
	return c.pick(ctx, chat, "That agent is unavailable. Your selection did not change.", 0)
}

func (c *telegramChannel) pick(ctx context.Context, chat telegramChat, lead string, page int) telegramResponse {
	link, err := c.server.store.GetTelegramLink(ctx, chat.userID)
	if err != nil {
		return telegramResponse{text: lead + "\n" + telegramListUnavailable}
	}
	recipients, err := c.recipientList(ctx, chat)
	if err != nil {
		return telegramResponse{text: lead + "\n" + telegramListUnavailable}
	}
	return telegramAgentChoices(chat, recipients, link.DefaultActorUID, lead, page)
}

func telegramAgentChoices(chat telegramChat, recipients []telegramRecipient, selected, lead string, page int) telegramResponse {
	if len(recipients) == 0 {
		return telegramResponse{text: lead + "\nNo agents are available. Start an accessible agent and try /use again."}
	}
	pages := (len(recipients)-1)/telegramChoicesPage + 1
	page = min(max(page, 0), pages-1)
	response := telegramResponse{text: lead}
	if selected != "" {
		current := "The selected agent is unavailable."
		for _, a := range recipients {
			if a.UID == selected {
				current = "Selected: " + a.path
				break
			}
		}
		response.text += "\n" + current
	}
	for _, a := range recipients[page*telegramChoicesPage : min((page+1)*telegramChoicesPage, len(recipients))] {
		label := a.path
		if a.UID == selected {
			label += " (selected)"
		}
		response.choices = append(response.choices, []telegramChoice{{label, telegramChoiceAction(chat, "a"+a.UID)}})
	}
	if pages > 1 {
		response.text += fmt.Sprintf("\nPage %d of %d.", page+1, pages)
		var nav []telegramChoice
		if page > 0 {
			nav = append(nav, telegramChoice{"Previous", telegramChoiceAction(chat, "p"+strconv.Itoa(page-1))})
		}
		if page+1 < pages {
			nav = append(nav, telegramChoice{"Next", telegramChoiceAction(chat, "p"+strconv.Itoa(page+1))})
		}
		response.choices = append(response.choices, nav)
	}
	return response
}

// telegramChoiceAction подписывает короткую ссылку случайным ключом
// привязки. UID (26 байт) и 128 бит подписи вместе с префиксом входят в 64
// байта callback_data. Ссылку старой привязки проверить новым ключом нельзя.
func telegramChoiceAction(chat telegramChat, action string) string {
	mac := hmac.New(sha256.New, []byte(chat.session))
	_, _ = mac.Write([]byte(telegramUseCallback + action))
	return telegramUseCallback + action + ":" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)[:16])
}

func (c *telegramChannel) useCallback(ctx context.Context, chat telegramChat, data string) (telegramResponse, bool) {
	action, _, ok := strings.Cut(strings.TrimPrefix(data, telegramUseCallback), ":")
	if !ok || !chat.active(ctx, c.server.store) || !hmac.Equal([]byte(data), []byte(telegramChoiceAction(chat, action))) {
		return telegramResponse{}, false
	}
	if uid, ok := strings.CutPrefix(action, "a"); ok && uid != "" {
		return c.selectRecipient(ctx, chat, func(a telegramRecipient) bool { return a.UID == uid }), true
	}
	if page, ok := strings.CutPrefix(action, "p"); ok {
		if n, err := strconv.Atoi(page); err == nil && n >= 0 {
			return c.pick(ctx, chat, "Choose an agent for your next messages.", n), true
		}
	}
	return telegramResponse{}, false
}

// toOwner отправляет сообщение агента владельцу его роя в привязанный чат.
// Ошибка несёт причину для отправителя, без подробностей Telegram.
func (c *telegramChannel) toOwner(ctx context.Context, from store.Agent, body string) error {
	return c.sendArtifact(ctx, from, body, "")
}

func (c *telegramChannel) sendArtifact(ctx context.Context, from store.Agent, body, versionID string) error {
	if strings.TrimSpace(body) == "" {
		return errors.New("message is empty")
	}
	sw, err := c.server.store.GetSwarm(ctx, from.SwarmID)
	if err != nil || sw.State != "active" {
		return errors.New("swarm is not active")
	}
	link, err := c.server.store.GetTelegramLink(ctx, sw.OwnerID)
	if err != nil || link.State != "linked" || link.ChatID == 0 {
		return errors.New("the swarm owner has no linked Telegram chat")
	}
	chat := telegramChat{userID: link.UserID, botUsername: link.BotUsername, chatID: link.ChatID}
	chat.session, err = c.server.store.TelegramChatSession(ctx, chat.userID, chat.botUsername, chat.chatID, chat.chatID)
	if err != nil || !chat.linked() {
		return errors.New("the Telegram link changed; no message was sent")
	}
	token, err := c.server.store.TelegramBotToken(ctx, sw.OwnerID, c.bots.install)
	if err != nil || token == "" {
		return errors.New("no Telegram bot is configured for the swarm owner")
	}
	client, err := telegram.NewClient(token, c.bots.client)
	if err != nil {
		return errors.New("the Telegram bot is misconfigured")
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if !chat.active(ctx, c.server.store) || !c.server.telegramOutboundBinding(ctx, chat.userID, chat.chatID) {
		return errors.New("the Telegram link changed; no message was sent")
	}
	if versionID != "" {
		return c.sendArtifactNotice(ctx, client, link, from, body, versionID)
	}
	text := strings.ToValidUTF8(from.Name+": "+body, "?")
	parts := telegram.SplitText(text)
	for i, part := range parts {
		if i > 0 && !chat.active(ctx, c.server.store) {
			return errors.New("the Telegram link changed; only part of the message was sent")
		}
		sent, err := client.SendMessage(ctx, link.ChatID, part, nil)
		if err != nil {
			return errors.New("Telegram did not confirm the message")
		}
		c.remember(link.ChatID, sent.MessageID, telegramRoute{session: chat.session, swarmID: sw.ID, actorUID: from.UID})
	}
	return nil
}

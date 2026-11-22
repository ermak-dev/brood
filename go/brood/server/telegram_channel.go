package server

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"brood.dev/go/brood/app"
	"brood.dev/go/brood/store"
	"brood.dev/go/brood/telegram"
)

// Telegram — канал шины между владельцем роя и его агентами. Привязанный
// чат пишет агенту роя: сообщение уходит тем же путём, что send оператора.
// Агент отвечает владельцу командой send на имя operator: текст приходит в
// чат владельца. Ответ на сообщение агента в Telegram адресуется этому
// агенту; иначе адресат — префикс "имя: текст" или единственный агент роя.
// Доставка в обе стороны разовая: отказ возвращается отправителю, очереди
// и повторов нет.

// telegramOperator — имя адресата шины, за которым стоит владелец роя.
const telegramOperator = "operator"

// telegramRouteLimit — сколько сообщений агентов чат помнит для адресации
// ответов; память процесса, после перезапуска адресат задаётся именем.
const telegramRouteLimit = 256

const telegramHelp = "Write a message to talk to an agent of your swarm. Reply to an agent's message to answer it, or start with \"name: \" to pick an agent.\n/swarms lists your swarms, /use NAME selects one, /agents lists its agents."

// telegramChat — привязанный чат обновления: пользователь, бот, чат и
// сессия привязки. Без сессии чат не привязан.
type telegramChat struct {
	userID, botUsername, session string
	chatID                       int64
}

func (c telegramChat) linked() bool { return c.session != "" }

// telegramClipText обрезает текст до предела сообщения Bot API.
func telegramClipText(text string, limit int) string {
	if telegram.TextLength(text) <= limit {
		return text
	}
	return telegram.SplitText(text)[0]
}

// telegramRoute — агент, чьё сообщение лежит в чате под данным номером.
type telegramRoute struct {
	swarmID, actor string
}

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
	c.routes[chatID][messageID] = route
	c.order[chatID] = append(c.order[chatID], messageID)
	for len(c.order[chatID]) > telegramRouteLimit {
		delete(c.routes[chatID], c.order[chatID][0])
		c.order[chatID] = c.order[chatID][1:]
	}
}

func (c *telegramChannel) route(chatID, messageID int64) (telegramRoute, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	route, ok := c.routes[chatID][messageID]
	return route, ok
}

// swarms — живые рои, которыми пользователь управляет: свои, у
// администратора — все.
func (c *telegramChannel) swarms(ctx context.Context, userID string) ([]store.Swarm, error) {
	user, err := c.server.store.GetUser(ctx, userID)
	if err != nil || user.Disabled {
		return nil, app.ErrNotAllowed
	}
	projects, err := c.server.gateway.Projects()
	if err != nil {
		return nil, err
	}
	var out []store.Swarm
	for _, project := range projects {
		rows, err := c.server.store.ListSwarmsByProject(ctx, project.ID)
		if err != nil {
			return nil, err
		}
		for _, sw := range rows {
			if sw.State == "active" && (user.IsAdmin || sw.OwnerID == userID) {
				out = append(out, sw)
			}
		}
	}
	return out, nil
}

// current — рой чата: выбранный /use, иначе единственный рой пользователя.
func (c *telegramChannel) current(ctx context.Context, chat telegramChat) (store.Swarm, string) {
	swarms, err := c.swarms(ctx, chat.userID)
	if err != nil {
		return store.Swarm{}, "Swarm list is unavailable; try again later."
	}
	if link, err := c.server.store.GetTelegramLink(ctx, chat.userID); err == nil && link.DefaultSwarmID != "" {
		for _, sw := range swarms {
			if sw.ID == link.DefaultSwarmID {
				return sw, ""
			}
		}
	}
	switch len(swarms) {
	case 0:
		return store.Swarm{}, "You have no active swarm."
	case 1:
		return swarms[0], ""
	}
	return store.Swarm{}, "You have several swarms; select one with /use NAME. " + telegramSwarmNames(swarms)
}

func telegramSwarmNames(swarms []store.Swarm) string {
	names := make([]string, 0, len(swarms))
	for _, sw := range swarms {
		names = append(names, sw.Name)
	}
	sort.Strings(names)
	return "Swarms: " + strings.Join(names, ", ")
}

// agents — имена работающих агентов роя.
func (c *telegramChannel) agents(ctx context.Context, swarmID string) ([]string, error) {
	rows, err := c.server.store.ListManagedActors(ctx, swarmID)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, a := range rows {
		if a.Phase == "running" && a.Desired == "running" {
			names = append(names, a.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// receive разбирает текст привязанного чата и возвращает ответ бота; пустой
// ответ — сообщение доставлено агенту, подтверждение не нужно.
func (c *telegramChannel) receive(ctx context.Context, chat telegramChat, m *telegram.Message) string {
	text := strings.TrimSpace(m.Text)
	if text == "" {
		return ""
	}
	if name, arg, ok := telegramCommand(text); ok {
		return c.command(ctx, chat, name, arg)
	}
	var sw store.Swarm
	target := ""
	var artifactReply store.TelegramArtifactReply
	if m.ReplyToMessage != nil {
		var err error
		artifactReply, err = c.server.store.GetTelegramArtifactReply(ctx, chat.session, m.ReplyToMessage.MessageID)
		if err == nil {
			found, swarmErr := c.server.store.GetSwarm(ctx, artifactReply.SwarmID)
			actor, actorErr := c.server.store.GetManagedActor(ctx, artifactReply.ActorUID)
			if swarmErr != nil || found.State != "active" || actorErr != nil || actor.SwarmID != found.ID || actor.Phase == "deleted" {
				return "The original recipient of this artifact reply is unavailable; nothing was sent."
			}
			sw, target = found, actor.Name
			text += "\n\n[brood] This operator reply is about artifact version " + c.server.artifactURL(sw.ID, artifactReply.VersionID)
		} else if !errors.Is(err, store.ErrNotFound) || strings.Contains(m.ReplyToMessage.Text, artifactNoticeMarker) {
			return "The version for this reply could not be confirmed; nothing was sent. Ask the agent to resend the artifact notice."
		} else if route, ok := c.route(chat.chatID, m.ReplyToMessage.MessageID); ok {
			found, err := c.server.store.GetSwarm(ctx, route.swarmID)
			if err == nil && found.State == "active" {
				sw, target = found, route.actor
			}
		}
	}
	if target == "" {
		var refusal string
		if sw, refusal = c.current(ctx, chat); refusal != "" {
			return refusal
		}
		names, err := c.agents(ctx, sw.ID)
		if err != nil {
			return "Agent list is unavailable; try again later."
		}
		if head, rest, ok := strings.Cut(text, ":"); ok {
			for _, name := range names {
				if strings.EqualFold(strings.TrimSpace(head), name) && strings.TrimSpace(rest) != "" {
					target, text = name, strings.TrimSpace(rest)
				}
			}
		}
		switch {
		case target != "":
		case len(names) == 1:
			target = names[0]
		case len(names) == 0:
			return fmt.Sprintf("Swarm %s has no running agent.", sw.Name)
		default:
			return "Several agents are running; start the message with \"name: \" or reply to an agent's message. Agents: " + strings.Join(names, ", ")
		}
	}
	// Право и живая привязка проверяются до отправки и после неё.
	session := func(ctx context.Context) bool {
		current, err := c.server.store.TelegramChatSession(ctx, chat.userID, chat.botUsername, chat.chatID, chat.chatID)
		return err == nil && current == chat.session
	}
	ctx = context.WithValue(ctx, commandSessionAuthorityKey{}, session)
	if !session(ctx) {
		return ""
	}
	resolve := func(ctx context.Context) (store.ManagedActor, error) {
		if artifactReply.ActorUID != "" {
			return c.server.store.GetManagedActor(ctx, artifactReply.ActorUID)
		}
		return c.server.store.LookupManagedActor(ctx, sw.ID, target)
	}
	_, _, err := c.server.deliverOperatorMessage(ctx, chat.userID, sw.ID, resolve, text, 0)
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errMessageNotSent):
		return fmt.Sprintf("%s did not take the message; it was not sent.", target)
	case errors.Is(err, errDeliveryUnconfirmed):
		return fmt.Sprintf("Delivery to %s is unconfirmed; check before sending again.", target)
	case errors.Is(err, errRecipientOffline):
		return fmt.Sprintf("%s is not connected.", target)
	case errors.Is(err, app.ErrNotFound):
		return fmt.Sprintf("No agent %s in swarm %s.", target, sw.Name)
	case errors.Is(err, app.ErrNotAllowed):
		return "You cannot message this swarm."
	}
	return "The message was not delivered; try again later."
}

func (c *telegramChannel) command(ctx context.Context, chat telegramChat, name, arg string) string {
	switch name {
	case "start", "help":
		return telegramHelp
	case "swarms":
		swarms, err := c.swarms(ctx, chat.userID)
		if err != nil {
			return "Swarm list is unavailable; try again later."
		}
		if len(swarms) == 0 {
			return "You have no active swarm."
		}
		return telegramSwarmNames(swarms)
	case "use":
		swarms, err := c.swarms(ctx, chat.userID)
		if err != nil {
			return "Swarm list is unavailable; try again later."
		}
		var matched []store.Swarm
		for _, sw := range swarms {
			if sw.ID == arg || sw.Name == arg {
				matched = append(matched, sw)
			}
		}
		if len(matched) != 1 {
			return "Name exactly one of your swarms: /use NAME. " + telegramSwarmNames(swarms)
		}
		session := func(ctx context.Context) bool {
			current, err := c.server.store.TelegramChatSession(ctx, chat.userID, chat.botUsername, chat.chatID, chat.chatID)
			return err == nil && current == chat.session
		}
		if _, err := c.server.store.SetTelegramSwarm(ctx, chat.userID, matched[0].ID, session); err != nil {
			return "The swarm was not selected; try again later."
		}
		return "Messages now go to swarm " + matched[0].Name + "."
	case "agents":
		sw, refusal := c.current(ctx, chat)
		if refusal != "" {
			return refusal
		}
		names, err := c.agents(ctx, sw.ID)
		if err != nil {
			return "Agent list is unavailable; try again later."
		}
		if len(names) == 0 {
			return fmt.Sprintf("Swarm %s has no running agent.", sw.Name)
		}
		return fmt.Sprintf("Agents of %s: %s", sw.Name, strings.Join(names, ", "))
	}
	return "Unknown command. " + telegramHelp
}

// toOwner отправляет сообщение агента владельцу его роя в привязанный чат.
// Ошибка несёт причину для отправителя, без подробностей Telegram.
func (c *telegramChannel) toOwner(ctx context.Context, from store.ManagedActor, body string) error {
	return c.sendArtifact(ctx, from, body, "")
}

func (c *telegramChannel) sendArtifact(ctx context.Context, from store.ManagedActor, body, versionID string) error {
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
	if versionID != "" {
		return c.sendArtifactNotice(ctx, client, link, from, body, versionID)
	}
	text := strings.ToValidUTF8(from.Name+": "+body, "?")
	for _, part := range telegram.SplitText(text) {
		sent, err := client.SendMessage(ctx, link.ChatID, part, nil)
		if err != nil {
			return errors.New("Telegram did not confirm the message")
		}
		c.remember(link.ChatID, sent.MessageID, telegramRoute{swarmID: sw.ID, actor: from.Name})
	}
	return nil
}

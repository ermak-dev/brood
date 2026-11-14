package server

// FEATURE: Actor and pass start commands from a linked Telegram chat (#284).
// SPEC: /actors lists the managed actors of a swarm through the catalog
// command agents, for every reader of the swarm. /actor <swarm> <name>
// stop|start|update|reset asks for confirmation and then runs the catalog
// command of the same name, reset with its confirmation argument, as the
// user of the link, through the same preflight as POST /api/commands: the
// manage right, an active swarm and its namespace. A refusal of the catalog
// carries its public reason, an internal failure only a generic text; a
// viewer is refused before any confirmation. A confirmation of /actor holds
// the actor and its generation: a press after the actor was reset or
// replaced does nothing. The link is checked before the catalog command and
// after it: a link that ended before runs nothing and gets no reply; a link
// that ended while the command ran is told that it may have taken effect,
// as is an unknown outcome; the command deadline is not taken for an
// unlink. A command runs within the command timeout: a Substrate that does
// not answer holds the poller no longer. /start <swarm> <goal> starts a new
// pass on the pinned workflow at the HEAD of the project repository after
// its confirmation, through the same server method as POST
// /api/swarms/{id}/start, and hands its first step to the step dispatcher as
// that route does; a swarm that never started is refused, since its workflow
// is chosen in brood. A confirmation of /stop holds the pass: a press after
// another pass started does nothing.
// IMPLEMENTATION: telegramCommands (telegram_commands.go), commandPreflight
// (command_http.go), startSwarmPass (swarm_execution_api.go).
// STATUS: DONE

import (
	"context"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"brood.dev/go/brood/store"
	"google.golang.org/grpc/codes"
)

// telegramConfig подключает к сборке поддельный Bot API с ботом установки и
// origin панели для ссылок.
func telegramConfig(api *fakeBotAPI) func(*Config) {
	return func(c *Config) {
		c.TelegramAPI = api.url
		c.telegramBotToken = installBotToken
		c.PanelOrigin = "https://brood.example"
	}
}

// linkInstallationChat привязывает личный чат chat пользователя client через
// бот установки, как это делает человек: код из профиля и /start <код>.
// Уведомления пользователя выключены: в чат приходят только ответы на
// команды.
func linkInstallationChat(t *testing.T, ts *testServer, api *fakeBotAPI, client *http.Client, username string, chat int64) telegramTestChat {
	t.Helper()
	resp, data, _ := ts.do(client, "POST", "/api/me/telegram/link", ts.csrf(client), nil)
	expectStatus(t, "POST /api/me/telegram/link", resp, data, http.StatusCreated)
	c := telegramTestChat{t: t, api: api, token: installBotToken, id: chat}
	if got := c.send("/start " + data["code"].(string)); len(got) != 1 || got[0].Text != telegramReplyLinked {
		t.Fatalf("linking chat %d: %+v", chat, got)
	}
	user, err := ts.srv.store.GetUserByUsername(context.Background(), username)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.srv.store.SetTelegramEvents(context.Background(), user.ID, []string{}); err != nil {
		t.Fatal(err)
	}
	return c
}

// telegramActorFixture — managed-рой alpha alice (aliceID) с работающим
// актором coding (uid) и привязанным чатом alice; bob — участник проекта без
// роли в рое; pve — поддельный Proxmox.
type telegramActorFixture struct {
	ts         *substrateServer
	api        *fakeBotAPI
	pve        *managedFakePVE
	alice, bob *http.Client
	chat       telegramTestChat
	swarm, uid string
	aliceID    string
}

func telegramActorSetup(t *testing.T) telegramActorFixture {
	t.Helper()
	api := newTestBotAPI(t)
	ts, alice, bob, swarm, pve, hub := managedSetup(t, telegramConfig(api))
	body := map[string]any{"metadata": map[string]string{"name": "coding"}, "actorTemplate": map[string]string{"name": "claude-proxmox-brood-proxmox"}}
	r, b, _ := ts.do(alice, "POST", "/api/swarms/"+swarm+"/actors", ts.csrf(alice), body)
	expectStatus(t, "managed create", r, b, http.StatusCreated)
	uid := b["metadata"].(map[string]any)["uid"].(string)
	hub.mu.Lock()
	hub.connected = true
	hub.mu.Unlock()
	settleActor(t, ts, uid, actorRunning)
	chat := linkInstallationChat(t, ts.testServer, api, alice, "alice", 601)
	user, err := ts.srv.store.GetUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	return telegramActorFixture{ts: ts, api: api, pve: pve, alice: alice, bob: bob, chat: chat, swarm: swarm, uid: uid, aliceID: user.ID}
}

// actor — managed-актор coding фикстуры, как его видит хранилище.
func (f telegramActorFixture) actor(t *testing.T) store.ManagedActor {
	t.Helper()
	a, err := f.ts.srv.store.GetManagedActor(context.Background(), f.uid)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// unlink отвязывает чат alice; годится для горутины поллера.
func (f telegramActorFixture) unlink(hooks *hookErrors) func() {
	return func() {
		_, err := f.ts.srv.store.UnlinkTelegram(context.Background(), f.aliceID)
		hooks.add(err)
	}
}

func TestTelegramActorsListsManagedActors(t *testing.T) {
	chat := telegramActorSetup(t).chat
	expectText(t, "/actors alpha", chat.text("/actors alpha"),
		"Managed actors of swarm \"alpha\", page 1 of 1:\n- coding (role worker): running, generation 1")
	chat.text("/use alpha")
	expectText(t, "/actors with the default", chat.text("/actors"),
		"Managed actors of swarm \"alpha\", page 1 of 1:\n- coding (role worker): running, generation 1")
	expectText(t, "/actors of an unknown swarm", chat.text("/actors beta"),
		`No swarm "beta" among your swarms. Send /swarms for the list.`)
	if got := chat.text("/swarm alpha"); !strings.Contains(got, "\nManaged actors: 1") {
		t.Fatalf("/swarm alpha = %q", got)
	}
}

func TestTelegramActorLifecycleRunsTheCatalogAfterConfirmation(t *testing.T) {
	f := telegramActorSetup(t)
	ts, chat, uid := f.ts, f.chat, f.uid
	ask := chat.confirmation("/actor alpha coding stop")
	expectText(t, "stop confirmation", ask.Text, "Stop actor \"coding\" in swarm \"alpha\"? Its container stops; the workspace is kept.\n\nConfirm within 2 minutes.")
	if ask.Markup.InlineKeyboard[0][0].Text != "Stop actor" {
		t.Fatalf("buttons: %+v", ask.Markup)
	}
	if a, _ := ts.srv.store.GetManagedActor(context.Background(), uid); a.Desired != "running" {
		t.Fatalf("the actor changed before the confirmation: %+v", a)
	}
	got := chat.press("q1", ask, 0)
	if result := got.only(t); !strings.HasPrefix(result, `Stop of actor "coding" in swarm "alpha" was accepted: actor "coding" intent accepted`) {
		t.Fatalf("stop result: %q", result)
	}
	stopped := settleActor(t, ts, uid, actorStopped)
	again := chat.press("q2", ask, 0)
	expectText(t, "second press", again.answer, "This action already ran; it is not repeated.")
	if a, _ := ts.srv.store.GetManagedActor(context.Background(), uid); a.Revision != stopped.Revision || a.OperationID != stopped.OperationID {
		t.Fatalf("a repeated press ran the command again: %+v -> %+v", stopped, a)
	}
	// Отказ каталога доходит до чата своей публичной причиной.
	update := chat.confirmation("/actor alpha coding update")
	expectText(t, "update of a stopped actor", chat.press("q3", update, 0).only(t),
		`brood refused to update actor "coding" in swarm "alpha": update refused: actor "coding" is stopped; update restarts a running actor; start it with start, which installs the server's current guest release`)
	start := chat.confirmation("/actor alpha coding start")
	if result := chat.press("q4", start, 0).only(t); !strings.HasPrefix(result, `Start of actor "coding" in swarm "alpha" was accepted:`) {
		t.Fatalf("start result: %q", result)
	}
	running := settleActor(t, ts, uid, actorRunning)
	reset := chat.confirmation("/actor alpha coding reset")
	expectText(t, "reset confirmation", reset.Text, "Reset actor \"coding\" in swarm \"alpha\"? Its container and workspace are destroyed and a new generation starts; this cannot be undone.\n\nConfirm within 2 minutes.")
	if result := chat.press("q5", reset, 0).only(t); !strings.HasPrefix(result, `Reset of actor "coding" in swarm "alpha" was accepted:`) {
		t.Fatalf("reset result: %q", result)
	}
	replaced := settleActor(t, ts, uid, actorRunning)
	if replaced.Incarnation <= running.Incarnation {
		t.Fatalf("reset kept the generation: %d -> %d", running.Incarnation, replaced.Incarnation)
	}
}

func TestTelegramActorCommandsNeedTheManageRight(t *testing.T) {
	f := telegramActorSetup(t)
	ts, alice, uid := f.ts, f.chat, f.uid
	bob := linkInstallationChat(t, ts.testServer, f.api, f.bob, "bob", 602)
	expectText(t, "bob without a role", bob.text("/actors alpha"), `No swarm "alpha" among your swarms. Send /swarms for the list.`)
	expectText(t, "bob without a role", bob.text("/actor alpha coding stop"), `No swarm "alpha" among your swarms. Send /swarms for the list.`)
	r, b, _ := ts.do(f.alice, "POST", "/api/swarms/"+f.swarm+"/members", ts.csrf(f.alice), map[string]any{"username": "bob", "role": "viewer"})
	if r.StatusCode >= 300 {
		t.Fatalf("grant viewer: %d %v", r.StatusCode, b)
	}
	if got := bob.text("/actors alpha"); !strings.Contains(got, "- coding (role worker): running") {
		t.Fatalf("viewer /actors = %q", got)
	}
	expectText(t, "viewer /actor", bob.text("/actor alpha coding stop"),
		`Changing actor "coding" in swarm "alpha" needs the owner or editor role in it; you are a viewer.`)
	if a, _ := ts.srv.store.GetManagedActor(context.Background(), uid); a.Desired != "running" {
		t.Fatalf("a viewer changed the actor: %+v", a)
	}
	expectText(t, "unknown action", alice.text("/actor alpha coding wipe"), `Unknown actor action "wipe": use stop, start, update or reset.`)
	expectText(t, "missing arguments", alice.text("/actor alpha"), "Name a swarm, an actor and an action: /actor <swarm> <name> stop|start|update|reset.")
	expectText(t, "unknown actor", alice.text("/actor alpha ghost stop"), `No managed actor "ghost" in swarm "alpha". Send /actors alpha for the list.`)
}

// Привязка, которая умерла после проверки поллера, пока нажатие ждало ответа
// Bot API, команду каталога не запускает и итога не получает.
func TestTelegramActorLinkEndedBeforeThePressRunsNothing(t *testing.T) {
	f := telegramActorSetup(t)
	ask := f.chat.confirmation("/actor alpha coding stop")
	before := f.actor(t)
	var hooks hookErrors
	f.api.scriptAnswer(installBotToken, f.unlink(&hooks))
	got := f.chat.press("q1", ask, 0)
	hooks.check(t)
	after := f.actor(t)
	if len(got.sent) != 0 || after.Desired != "running" || after.Revision != before.Revision {
		t.Fatalf("a press of an ended link changed the actor: %+v, %+v -> %+v", got, before, after)
	}
}

// Отозванная до записи сессия запрещает намерение (#299). Адаптер пока
// предупреждает об умершей привязке даже при отказе жизненного цикла.
func TestTelegramActorChangeOverALinkThatEndedSaysItMayHaveTakenEffect(t *testing.T) {
	f := telegramActorSetup(t)
	ask := f.chat.confirmation("/actor alpha coding stop")
	before := f.actor(t)
	var hooks hookErrors
	f.ts.srv.telegram.commands.beforeWrite = f.unlink(&hooks)
	got := f.chat.press("q1", ask, 0).only(t)
	hooks.check(t)
	expectText(t, "stop over an ended link", got,
		`This chat is no longer linked to your brood account: the stop of actor "coding" in swarm "alpha" may have taken effect. Check the actor in brood.`)
	if a := f.actor(t); a.Desired != before.Desired || a.Revision != before.Revision || a.OperationID != before.OperationID {
		t.Fatalf("revoked chat changed the actor: %+v -> %+v", before, a)
	}
}

func TestTelegramActorUnknownOutcomeIsNamedUnknown(t *testing.T) {
	f := telegramActorSetup(t)
	f.ts.srv.artifactsDir = writeGuestArtifacts(t)
	guestReleaseHub(f.ts, reportsRelease(oldRelease))
	f.pve.mu.Lock()
	f.pve.uncertainStop = true
	f.pve.mu.Unlock()
	ask := f.chat.confirmation("/actor alpha coding update")
	expectText(t, "unknown update", f.chat.press("q1", ask, 0).only(t),
		`The outcome of the update of actor "coding" in swarm "alpha" is unknown: it may have taken effect. Check the actor in brood before you try again.`)
}

// Команда, которая упёрлась в свой срок, всё равно получает ответ: проверка
// привязки после неё идёт без срока команды.
func TestTelegramActorCommandPastItsDeadlineStillAnswers(t *testing.T) {
	f := telegramActorSetup(t)
	f.ts.srv.telegram.commands.timeout = 2 * time.Second
	f.pve.mu.Lock()
	f.pve.afterStop = func() { time.Sleep(2500 * time.Millisecond) }
	f.pve.mu.Unlock()
	ask := f.chat.confirmation("/actor alpha coding stop")
	if got := f.chat.press("q1", ask, 0).only(t); !strings.HasPrefix(got, `Stop of actor "coding" in swarm "alpha" was accepted:`) {
		t.Fatalf("stop past the deadline: %q", got)
	}
}

// Команда идёт в срок команды: Substrate, который не отвечает на проверку
// namespace, держит поллер не дольше этого срока, и чат получает общий отказ.
func TestTelegramCommandRunsWithinTheCommandTimeout(t *testing.T) {
	f := telegramActorSetup(t)
	c := f.ts.srv.telegram.commands
	c.timeout = 200 * time.Millisecond
	session, err := f.ts.srv.store.TelegramChatSession(context.Background(), f.aliceID, "", f.chat.id, f.chat.id)
	if err != nil || session == "" {
		t.Fatalf("session of the chat: %q %v", session, err)
	}
	chat := telegramChat{userID: f.aliceID, chatID: f.chat.id, session: session}
	release := make(chan struct{})
	f.ts.fake.mu.Lock()
	f.ts.fake.beforeNamespaceGet = func() {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
	}
	f.ts.fake.mu.Unlock()
	start := time.Now()
	reply := c.run(context.Background(), chat, "/actors alpha")
	elapsed := time.Since(start)
	close(release)
	if elapsed > 5*time.Second {
		t.Fatalf("the command held the poller for %s past its timeout of %s", elapsed, c.timeout)
	}
	expectText(t, "/actors past the command timeout", reply.text,
		`brood could not list the actors of swarm "alpha" right now; nothing is known to have changed. Try again later.`)
}

func TestTelegramCatalogInternalFailureKeepsItsTextOut(t *testing.T) {
	f := telegramActorSetup(t)
	ask := f.chat.confirmation("/actor alpha coding stop")
	f.ts.fake.mu.Lock()
	f.ts.fake.namespaceError = grpcErr(codes.Internal, "SENTINEL internal failure")
	f.ts.fake.mu.Unlock()
	expectText(t, "/actors over a failure", f.chat.text("/actors alpha"),
		`brood could not list the actors of swarm "alpha" right now; nothing is known to have changed. Try again later.`)
	expectText(t, "stop over a failure", f.chat.press("q1", ask, 0).only(t),
		`brood could not stop actor "coding" in swarm "alpha" right now; nothing is known to have changed. Try again later.`)
	if a := f.actor(t); a.Desired != "running" {
		t.Fatalf("a failed stop changed the actor: %+v", a)
	}
}

// Рой, который перестал быть активным, отвергают проверки маршрута
// команд каталога: и чтение, и подтверждённое до этого изменение.
func TestTelegramActorCommandsNeedAnActiveSwarm(t *testing.T) {
	f := telegramActorSetup(t)
	ask := f.chat.confirmation("/actor alpha coding stop")
	execDB(t, f.ts, `UPDATE swarms SET state = 'error' WHERE id = ?`, f.swarm)
	expectText(t, "/actors of an inactive swarm", f.chat.text("/actors alpha"),
		`brood refused to list the actors of swarm "alpha": swarm is not active`)
	expectText(t, "/actor of an inactive swarm", f.chat.text("/actor alpha coding start"),
		`Swarm "alpha" is error: it cannot be changed until it is active.`)
	expectText(t, "stop confirmed while active", f.chat.press("q1", ask, 0).only(t),
		`brood refused to stop actor "coding" in swarm "alpha": swarm not found`)
	if a := f.actor(t); a.Desired != "running" {
		t.Fatalf("a refused stop changed the actor: %+v", a)
	}
}

// Подтверждение держит актора и его поколение: reset, подтверждённый до
// чужого reset, новое поколение не трогает.
func TestTelegramActorConfirmationHoldsTheGeneration(t *testing.T) {
	f := telegramActorSetup(t)
	first := f.chat.confirmation("/actor alpha coding reset")
	second := f.chat.confirmation("/actor alpha coding reset")
	if result := f.chat.press("q1", second, 0).only(t); !strings.HasPrefix(result, `Reset of actor "coding" in swarm "alpha" was accepted:`) {
		t.Fatalf("reset result: %q", result)
	}
	replaced := settleActor(t, f.ts, f.uid, actorRunning)
	expectText(t, "reset of the earlier generation", f.chat.press("q2", first, 0).only(t),
		`Actor "coding" in swarm "alpha" was reset or replaced since you confirmed; nothing was done. Send the command again to act on the actor as it is now.`)
	if a := f.actor(t); a.Incarnation != replaced.Incarnation || a.Revision != replaced.Revision {
		t.Fatalf("a stale reset changed the actor: %+v -> %+v", replaced, a)
	}
}

// telegramStartSetup — managed-рой alpha alice над Git-репозиторием, чей
// HEAD — head, с привязанным чатом alice.
func telegramStartSetup(t *testing.T) (f startAPIFixture, chat telegramTestChat, head, aliceID string) {
	t.Helper()
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	home := t.TempDir()
	_, bare, backend := seededGitBackend(t, gitPath, home)
	head = strings.TrimSpace(mustGit(t, home, bare, "rev-parse", "main"))
	api := newTestBotAPI(t)
	f, _ = resolveStartSetup(t, nil, backend, telegramConfig(api))
	chat = linkInstallationChat(t, f.ts.testServer, api, f.alice, "alice", 701)
	user, err := f.ts.srv.store.GetUserByUsername(context.Background(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	return f, chat, head, user.ID
}

// Подтверждение /stop держит проход: после чужой остановки и нового
// прохода нажатие его не останавливает.
func TestTelegramStopConfirmationHoldsThePass(t *testing.T) {
	f, chat, _, _ := telegramStartSetup(t)
	r, b, _ := f.startRaw(t, map[string]any{"goal": startAPIGoal, "definition": "approve"})
	expectStatus(t, "first start", r, b, http.StatusOK)
	ask := chat.confirmation("/stop alpha")
	r, b = f.stop(f.alice)
	expectStatus(t, "stop", r, b, http.StatusOK)
	r, b, _ = f.startRaw(t, map[string]any{"goal": "Another goal", "definition": "approve"})
	expectStatus(t, "second start", r, b, http.StatusOK)
	expectText(t, "stop of an earlier pass", chat.press("q1", ask, 0).only(t),
		`Swarm "alpha" started another pass since you confirmed; nothing was done. Send /stop again to stop the pass that runs now.`)
	if sw, err := f.ts.srv.store.GetSwarm(context.Background(), f.swarm); err != nil || sw.Execution != "waiting" {
		t.Fatalf("swarm after a stale stop: %+v %v", sw, err)
	}
}

// Привязка, которая умерла после всех проверок адаптера, перед самой
// записью, — отказ транзакции запуска: ни прохода, ни итога.
func TestTelegramStartChecksTheLinkInItsWrite(t *testing.T) {
	f, chat, _, aliceID := telegramStartSetup(t)
	r, b, _ := f.startRaw(t, map[string]any{"goal": startAPIGoal, "definition": "approve"})
	expectStatus(t, "first start", r, b, http.StatusOK)
	r, b = f.stop(f.alice)
	expectStatus(t, "stop", r, b, http.StatusOK)
	steps, err := f.ts.srv.store.SwarmSteps(context.Background(), f.swarm)
	if err != nil {
		t.Fatal(err)
	}
	ask := chat.confirmation("/start alpha Ship the next build")
	var hooks hookErrors
	f.ts.srv.telegram.commands.beforeWrite = func() {
		_, err := f.ts.srv.store.UnlinkTelegram(context.Background(), aliceID)
		hooks.add(err)
	}
	got := chat.press("q1", ask, 0)
	hooks.check(t)
	after, err := f.ts.srv.store.SwarmSteps(context.Background(), f.swarm)
	if err != nil || len(got.sent) != 0 || len(after) != len(steps) {
		t.Fatalf("a start over an ended link: %+v, %d -> %d steps, %v", got, len(steps), len(after), err)
	}
}

// Первый шаг прохода, начатого из Telegram, уходит диспетчеру шагов, как
// после запуска из панели: проход на графе, чей вход — узел актора, не ждёт
// в pending перезапуска сервера.
func TestTelegramStartDispatchesTheFirstActorStep(t *testing.T) {
	f, chat, _, _ := telegramStartSetup(t)
	ctx := context.Background()
	if err := f.ts.srv.store.BindSwarmRole(ctx, store.SwarmRoleBinding{SwarmID: f.swarm, Role: "review", ActorUID: f.master.UID}); err != nil {
		t.Fatal(err)
	}
	r, b, _ := f.startRaw(t, map[string]any{"goal": startAPIGoal, "definition": "review"})
	expectStatus(t, "first start", r, b, http.StatusOK)
	f.ts.srv.steps.wait()
	r, b = f.stop(f.alice)
	expectStatus(t, "stop", r, b, http.StatusOK)
	ask := chat.confirmation("/start alpha Review the next build")
	if result := chat.press("q1", ask, 0).only(t); !strings.HasPrefix(result, `Swarm "alpha" started a new pass on workflow review: execution running`) {
		t.Fatalf("start result: %q", result)
	}
	f.ts.srv.steps.wait()
	steps, err := f.ts.srv.store.SwarmSteps(ctx, f.swarm)
	if err != nil || len(steps) != 2 {
		t.Fatalf("steps after /start: %+v %v", steps, err)
	}
	if first := steps[1]; first.NodeID != "check" || first.State == "pending" {
		t.Fatalf("the first step of the pass from Telegram was not dispatched: %+v", first)
	}
}

// FEATURE: /start of a pass from Telegram resolves HEAD as the panel does.
func TestTelegramStartRunsANewPassAfterConfirmation(t *testing.T) {
	f, chat, head, _ := telegramStartSetup(t)
	expectText(t, "never started", chat.text("/start alpha Ship the next build"),
		`Swarm "alpha" has not run a pass yet: start its first pass in brood, where you choose its workflow.`)
	r, b, _ := f.startRaw(t, map[string]any{"goal": startAPIGoal, "definition": "approve"})
	expectStatus(t, "first start", r, b, http.StatusOK)
	r, b = f.stop(f.alice)
	expectStatus(t, "stop", r, b, http.StatusOK)
	expectText(t, "/start without a goal", chat.text("/start alpha"), "Name the goal of the pass: /start <swarm> <goal>.")
	goal := "Ship the next build\nwith release notes"
	ask := chat.confirmation("/start alpha " + goal)
	expectText(t, "start confirmation", ask.Text,
		"Start a new pass of swarm \"alpha\" (project proj) on workflow approve, at the current HEAD of the project repository?\n\nGoal:\n"+goal+"\n\nConfirm within 2 minutes.")
	result := chat.press("q1", ask, 0).only(t)
	if !strings.HasPrefix(result, `Swarm "alpha" started a new pass on workflow approve: execution waiting`) {
		t.Fatalf("start result: %q", result)
	}
	sw, err := f.ts.srv.store.GetSwarm(context.Background(), f.swarm)
	if err != nil || sw.Execution != "waiting" || sw.Source == nil || sw.Source.Commit != head {
		t.Fatalf("swarm after /start: %+v %v", sw, err)
	}
	steps, err := f.ts.srv.store.SwarmSteps(context.Background(), f.swarm)
	if err != nil || currentPassGoal(steps) != goal {
		t.Fatalf("goal of the new pass = %q, %v", currentPassGoal(steps), err)
	}
	// Повтор подтверждения второго прохода не начинает.
	again := chat.press("q2", ask, 0)
	expectText(t, "repeated start", again.answer, "This action already ran; it is not repeated.")
	if after, _ := f.ts.srv.store.SwarmSteps(context.Background(), f.swarm); len(after) != len(steps) {
		t.Fatalf("a repeated press started another pass: %d -> %d steps", len(steps), len(after))
	}
	// Пока проход идёт, следующий запуск отвергает запись с её причиной.
	busy := chat.confirmation("/start alpha another goal")
	expectText(t, "start while running", chat.press("q3", busy, 0).only(t),
		`brood refused to start swarm "alpha": swarm state conflict: execution is waiting; start requires idle, succeeded, failed or stopped`)
}

package store

// swarmRequestsSchema хранит запросы оператору и его ответы как отдельную
// сущность роя (#209). Узловой запрос (origin='node') — человеческий шаг
// графа: исход выбирает маршрут, свободный текст маршрута не касается.
// Запрос актора (origin='actor') — вопрос по его инициативе: граф он не
// продвигает, исхода у него нет. Подпись нажатого варианта восстановима из
// options по сохранённому исходу. CHECK держат правила и при записи мимо
// кода: транзакционная проверка в Go — первая линия, схема — вторая.
// Регистрация — schemaModules ("swarm-requests") и
// verifySwarmRequestsSchema в registry.go (#205).
const swarmRequestsSchema = `CREATE TABLE swarm_requests (
    id TEXT PRIMARY KEY,
    swarm_id TEXT NOT NULL REFERENCES swarms(id) ON DELETE CASCADE,
    sequence INTEGER,
    origin TEXT NOT NULL CHECK (origin IN ('node', 'actor')),
    actor_uid TEXT,
    actor_incarnation INTEGER,
    kind TEXT NOT NULL CHECK (kind IN ('choice', 'text', 'confirm')),
    question TEXT NOT NULL,
    context TEXT,
    options BLOB,
    state TEXT NOT NULL CHECK (state IN ('open', 'answered', 'cancelled')),
    created_at TEXT NOT NULL,
    answered_at TEXT,
    answered_by TEXT REFERENCES users(id),
    outcome TEXT,
    response TEXT,
    FOREIGN KEY (swarm_id, sequence) REFERENCES swarm_steps(swarm_id, sequence),
    CHECK (origin <> 'node' OR (sequence IS NOT NULL AND kind = 'choice' AND actor_uid IS NULL)),
    CHECK (origin <> 'actor' OR (sequence IS NULL AND actor_uid IS NOT NULL AND actor_incarnation > 0 AND outcome IS NULL)),
    CHECK (origin <> 'actor' OR length(question) > 0),
    CHECK (kind <> 'choice' OR options IS NOT NULL),
    CHECK (state = 'answered' OR (answered_at IS NULL AND answered_by IS NULL AND outcome IS NULL AND response IS NULL)),
    CHECK (state <> 'answered' OR (answered_at IS NOT NULL AND answered_by IS NOT NULL)),
    CHECK (origin <> 'node' OR state <> 'answered' OR outcome IS NOT NULL)
)`

// swarmRequestOpenNode: граф ждёт оператора не более чем по одному запросу.
const swarmRequestOpenNode = `CREATE UNIQUE INDEX swarm_requests_open_node ON swarm_requests(swarm_id) WHERE origin = 'node' AND state = 'open'`

// swarmRequestStep: у посещения узла один запрос.
const swarmRequestStep = `CREATE UNIQUE INDEX swarm_requests_step ON swarm_requests(swarm_id, sequence) WHERE origin = 'node'`

// swarmRequestFence: после ответа или отмены строка не меняется, а у
// открытой не меняется личность: ответ нельзя переписать и перенести.
const swarmRequestFence = `CREATE TRIGGER swarm_requests_fence BEFORE UPDATE ON swarm_requests WHEN OLD.state <> 'open' OR NEW.id IS NOT OLD.id OR NEW.swarm_id IS NOT OLD.swarm_id OR NEW.sequence IS NOT OLD.sequence OR NEW.origin IS NOT OLD.origin OR NEW.actor_uid IS NOT OLD.actor_uid OR NEW.actor_incarnation IS NOT OLD.actor_incarnation OR NEW.kind IS NOT OLD.kind OR NEW.question IS NOT OLD.question OR NEW.context IS NOT OLD.context OR NEW.options IS NOT OLD.options OR NEW.created_at IS NOT OLD.created_at BEGIN SELECT RAISE(ABORT, 'swarm request is immutable'); END`

// swarmRequestNoDelete: запрос живёт, пока жив его рой. Каскад удаления роя
// проходит: к моменту удаления дочерних строк родителя в swarms уже нет.
// Для REPLACE при recursive_triggers=OFF триггер удаления не срабатывает,
// его закрывает swarmRequestNoReplace.
const swarmRequestNoDelete = `CREATE TRIGGER swarm_requests_no_delete BEFORE DELETE ON swarm_requests WHEN EXISTS (SELECT 1 FROM swarms WHERE id=OLD.swarm_id) BEGIN SELECT RAISE(ABORT, 'swarm request is permanent'); END`

// swarmRequestNoReplace: вставка не может занять место существующей строки
// ни по id, ни по ключу шага, ни по слоту единственного открытого узлового
// запроса: INSERT OR REPLACE стёр бы ответ мимо триггера удаления.
const swarmRequestNoReplace = `CREATE TRIGGER swarm_requests_no_replace BEFORE INSERT ON swarm_requests WHEN EXISTS (SELECT 1 FROM swarm_requests WHERE id=NEW.id OR (NEW.origin='node' AND origin='node' AND swarm_id=NEW.swarm_id AND (sequence IS NEW.sequence OR (state='open' AND NEW.state='open')))) BEGIN SELECT RAISE(ABORT, 'swarm request is permanent'); END`

// swarmRequestCancelGuard: узловой запрос отменяется только вместе с
// остановкой графа. Прямая отмена при идущем графе оставила бы шаг в
// waiting без запроса, на который можно ответить.
const swarmRequestCancelGuard = `CREATE TRIGGER swarm_requests_cancel_guard BEFORE UPDATE OF state ON swarm_requests WHEN NEW.state = 'cancelled' AND OLD.origin = 'node' AND EXISTS (SELECT 1 FROM swarms WHERE id=OLD.swarm_id AND execution IN ('running', 'waiting')) BEGIN SELECT RAISE(ABORT, 'node request is closed by stopping the graph'); END`

// swarmGateStop: граф перестал идти — ждущий шлюз закрывается тем же
// оператором, что сменил исполнение, при любой записи.
const swarmGateStop = `CREATE TRIGGER swarms_gate_stop AFTER UPDATE OF execution ON swarms WHEN OLD.execution IN ('running', 'waiting') AND NEW.execution NOT IN ('running', 'waiting') BEGIN UPDATE swarm_requests SET state='cancelled' WHERE swarm_id=NEW.id AND origin='node' AND state='open'; END`

// swarmGateResume: остановленный граф с ждущим шагом без открытого запроса
// не возобновляется: шлюз отменён, а отменённое не оживает.
const swarmGateResume = `CREATE TRIGGER swarms_gate_resume BEFORE UPDATE OF execution ON swarms WHEN NEW.execution IN ('running', 'waiting') AND OLD.execution NOT IN ('running', 'waiting') AND EXISTS (SELECT 1 FROM swarm_steps s WHERE s.swarm_id=OLD.id AND s.state='waiting' AND NOT EXISTS (SELECT 1 FROM swarm_requests q WHERE q.swarm_id=s.swarm_id AND q.sequence=s.sequence AND q.origin='node' AND q.state='open')) BEGIN SELECT RAISE(ABORT, 'graph waits on a cancelled gate'); END`

func swarmRequestsFullSchema() string {
	return swarmRequestsSchema + ";" + swarmRequestOpenNode + ";" + swarmRequestStep + ";" + swarmRequestFence +
		";" + swarmRequestNoDelete + ";" + swarmRequestNoReplace + ";" + swarmRequestCancelGuard +
		";" + swarmGateStop + ";" + swarmGateResume
}

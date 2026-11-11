package store

// swarmStepsSchema хранит по одной строке на посещение узла роя (#206):
// ключ (swarm_id, sequence) назначается прикладным кодом. Доступ
// проверяет API: администратору не обязательно состоять в рое. Удаление
// роя каскадно удаляет его шаги вместе со снимком графа.
// Словарь состояний закрыт схемой. Шаг actor-узла идёт pending → attesting
// (резерв попытки) → delivering (workspace подтверждён) → inflight
// (инструкция доставлена) → completed; unconfirmed — исход эффекта у гостя
// неизвестен, refused — инструкция заведомо не ушла (#212). attested_commit
// — commit, который подтвердил гость для этой попытки (#183); detail —
// причина отказа или неопределённости для оператора. report — отчёт актора
// о шаге (#233): есть только у завершённого шага, пишется вместе с исходом
// и не меняется (swarmStepReportFence); предел — MaxStepReportBytes. goal —
// задача прохода (#242): есть только у первого шага прохода, пишется тем же
// INSERT, что открывает проход (beginPass), и не меняется
// (swarmStepPassFence); предел — MaxSwarmGoalBytes. source_commit — commit
// источника прохода (#229): живёт рядом с goal, тем же INSERT и той же
// оградой, у первого шага прохода есть оба или ни одного. Шаги прохода — его
// первый шаг и следующие до первого шага следующего прохода.
// Регистрация в общей схеме — schemaModules ("pipelines") и
// verifySwarmStepsSchema в registry.go (#205); правило «шаг роя с
// источником уходит актору только подтверждённым» — swarmStepAttested в
// модуле источника: триггер читает pipeline_sources и commit прохода шага.
const swarmStepsSchema = `CREATE TABLE swarm_steps (
    swarm_id TEXT NOT NULL REFERENCES swarms(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL,
    node_id TEXT NOT NULL,
    state TEXT NOT NULL CHECK (state IN ('pending', 'attesting', 'delivering', 'inflight', 'unconfirmed', 'refused', 'waiting', 'answered', 'completed', 'stopped')),
    started_at TEXT,
    finished_at TEXT,
    outcome TEXT,
    actor_uid TEXT,
    actor_incarnation INTEGER,
    request_id TEXT,
    attested_commit TEXT CHECK (attested_commit IS NULL OR (length(attested_commit) = 40 AND attested_commit NOT GLOB '*[^0-9a-f]*')),
    detail TEXT CHECK (detail IS NULL OR length(CAST(detail AS BLOB)) BETWEEN 1 AND 1024),
    report TEXT CHECK (report IS NULL OR length(CAST(report AS BLOB)) BETWEEN 1 AND 16384),
    goal TEXT CHECK (goal IS NULL OR length(CAST(goal AS BLOB)) BETWEEN 1 AND 4096),
    source_commit TEXT CHECK (source_commit IS NULL OR (length(source_commit) = 40 AND source_commit NOT GLOB '*[^0-9a-f]*')),
    PRIMARY KEY (swarm_id, sequence),
    UNIQUE (swarm_id, request_id),
    CHECK (report IS NULL OR (state = 'completed' AND outcome IS NOT NULL)),
    CHECK ((goal IS NULL) = (source_commit IS NULL))
)`

// swarmStepReportFence: отчёт пишется только тем UPDATE, что завершает шаг,
// и после не меняется и не стирается. Вместе с CHECK «отчёт только у
// завершённого шага» это и есть «один раз вместе с исходом» мимо кода.
const swarmStepReportFence = `CREATE TRIGGER swarm_steps_report_fence BEFORE UPDATE ON swarm_steps WHEN NEW.report IS NOT OLD.report AND (OLD.report IS NOT NULL OR OLD.state = 'completed') BEGIN SELECT RAISE(ABORT, 'step report is written once with the outcome'); END`

// swarmStepPassFence: задача и commit прохода живут только в той строке, что
// открыла проход, и с той вставки не переписываются, не стираются и не
// дописываются другому шагу. Поэтому повторный запуск с другой задачей или
// commit не может поменять их у прошлого прохода, что бы ни делал код.
const swarmStepPassFence = `CREATE TRIGGER swarm_steps_pass_fence BEFORE UPDATE ON swarm_steps WHEN NEW.goal IS NOT OLD.goal OR NEW.source_commit IS NOT OLD.source_commit BEGIN SELECT RAISE(ABORT, 'pass goal and commit are written once with the first step of the pass'); END`

// swarmStepsFullSchema — таблица шагов и ограждения отчёта и прохода.
func swarmStepsFullSchema() string {
	return swarmStepsSchema + ";" + swarmStepReportFence + ";" + swarmStepPassFence
}

// swarmStepAttested: шаг роя со снимком источника уходит актору и принимает
// исход (delivering, inflight) только с подтверждённым commit своего прохода
// (#229): commit первого шага прохода, то есть последнего шага с commit не
// дальше этого, а у вставляемого первого шага — его собственный. Шаг вне
// прохода commit не имеет и не уходит. Вставка и любая правка проверяются
// одинаково: подтверждение не снимается и не подменяется мимо кода.
const swarmStepAttested = `CREATE TRIGGER swarm_steps_attested BEFORE UPDATE ON swarm_steps WHEN NEW.state IN ('delivering', 'inflight') AND EXISTS (SELECT 1 FROM pipeline_sources p WHERE p.swarm_id = NEW.swarm_id) AND ` + swarmStepPassCommit + ` IS NOT NEW.attested_commit BEGIN SELECT RAISE(ABORT, 'actor step requires a workspace attestation of its pass source'); END`

const swarmStepAttestedInsert = `CREATE TRIGGER swarm_steps_attested_insert BEFORE INSERT ON swarm_steps WHEN NEW.state IN ('delivering', 'inflight') AND EXISTS (SELECT 1 FROM pipeline_sources p WHERE p.swarm_id = NEW.swarm_id) AND ` + swarmStepPassCommit + ` IS NOT NEW.attested_commit BEGIN SELECT RAISE(ABORT, 'actor step requires a workspace attestation of its pass source'); END`

// swarmStepPassCommit — commit прохода шага NEW в теле триггера; то же
// правило, что у stepPass.
const swarmStepPassCommit = `COALESCE(NEW.source_commit, (SELECT s.source_commit FROM swarm_steps s WHERE s.swarm_id = NEW.swarm_id AND s.sequence <= NEW.sequence AND s.source_commit IS NOT NULL ORDER BY s.sequence DESC LIMIT 1))`

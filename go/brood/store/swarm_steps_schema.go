package store

// swarmStepsSchema хранит по одной строке на посещение узла роя (#206):
// ключ (swarm_id, sequence) назначается прикладным кодом. Доступ
// проверяет API: администратору не обязательно состоять в рое. Удаление
// роя каскадно удаляет его шаги вместе со снимком графа.
// Регистрация в общей схеме — schemaModules ("pipelines") и
// verifySwarmStepsSchema в registry.go (#205).
const swarmStepsSchema = `CREATE TABLE swarm_steps (
    swarm_id TEXT NOT NULL REFERENCES swarms(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL,
    node_id TEXT NOT NULL,
    state TEXT NOT NULL,
    started_at TEXT,
    finished_at TEXT,
    outcome TEXT,
    actor_uid TEXT,
    actor_incarnation INTEGER,
    request_id TEXT,
    native_actor_name TEXT,
    native_template_name TEXT,
    native_template_uid TEXT,
    native_template_digest TEXT,
    PRIMARY KEY (swarm_id, sequence),
    UNIQUE (swarm_id, request_id)
)`

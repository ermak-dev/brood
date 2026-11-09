package store

// pipelineSchema хранит снимок графа и отдельную строку для каждого посещения.
// Доступ проверяет API: администратору не обязательно состоять в рое.
// Удаление владельца каскадно удаляет его запуски, как и принадлежащие ему рои.
// Регистрация в общей схеме — schemaModules ("pipelines") и verifyPipelinesSchema
// в registry.go (#205).
const pipelineSchema = `CREATE TABLE pipeline_runs (
    id TEXT NOT NULL PRIMARY KEY,
    swarm_id TEXT NOT NULL REFERENCES swarms(id) ON DELETE CASCADE,
    owner_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    graph BLOB NOT NULL,
    status TEXT NOT NULL
);
CREATE TABLE pipeline_steps (
    run_id TEXT NOT NULL REFERENCES pipeline_runs(id) ON DELETE CASCADE,
    sequence INTEGER NOT NULL,
    node_id TEXT NOT NULL,
    state TEXT NOT NULL,
    answer TEXT,
    outcome TEXT,
    actor_uid TEXT,
    actor_incarnation INTEGER,
    request_id TEXT,
    native_actor_name TEXT,
    native_template_name TEXT,
    native_template_uid TEXT,
    native_template_digest TEXT,
    PRIMARY KEY (run_id, sequence),
    UNIQUE (run_id, request_id)
)`

CREATE TABLE actor_operations (
    actor_uid TEXT PRIMARY KEY NOT NULL REFERENCES managed_actors(uid) ON DELETE CASCADE,
    operation_id TEXT NOT NULL CHECK (operation_id <> ''),
    generation INTEGER NOT NULL CHECK (generation > 0),
    caller TEXT NOT NULL CHECK (caller <> ''),
    operation TEXT NOT NULL CHECK (operation IN ('restart', 'slash')),
    owner INTEGER NOT NULL CHECK (owner IN (0, 1)),
    state TEXT NOT NULL CHECK (state IN ('inflight', 'unconfirmed'))
)

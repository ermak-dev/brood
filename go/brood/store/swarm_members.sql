CREATE TABLE swarm_members (
    swarm_id TEXT NOT NULL REFERENCES swarms(id) ON DELETE CASCADE,
    user_id  TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role     TEXT NOT NULL CHECK (role IN ('owner', 'editor', 'viewer')),
    PRIMARY KEY (swarm_id, user_id)
);

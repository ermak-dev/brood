CREATE TABLE project_capacity (
    project_id TEXT PRIMARY KEY NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    worker_selector TEXT NOT NULL CHECK (json_valid(worker_selector) AND json_type(worker_selector) = 'object'),
    max_swarms INTEGER CHECK (max_swarms >= 0),
    max_actors INTEGER CHECK (max_actors >= 0),
    max_cpu_millicores INTEGER CHECK (max_cpu_millicores >= 0),
    max_memory_bytes INTEGER CHECK (max_memory_bytes >= 0)
);

ALTER TABLE swarms ADD COLUMN retry TEXT NOT NULL DEFAULT '' CHECK (retry IN ('', 'create', 'delete'));

-- The import journal tracks in-progress data imports so a crashed, canceled, or lease-losing import is converged (rolled back or completed) before the instance serves traffic again
-- IF NOT EXISTS is required because the import command creates this table itself before it replaces the schema, and the table is the one Pocket ID table preserved while an import rebuilds every other one
CREATE TABLE IF NOT EXISTS import_journal
(
    generation_id   TEXT NOT NULL PRIMARY KEY,
    token           TEXT NOT NULL,
    owner_id        TEXT NOT NULL,
    owner_deadline  TIMESTAMPTZ NOT NULL,
    phase           TEXT NOT NULL,
    target_version  BIGINT NOT NULL,
    actors_included BOOLEAN NOT NULL,
    old_actors      BYTEA,
    work_dir        TEXT,
    uploads_state   TEXT,
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL,
    updated_at      TIMESTAMPTZ NOT NULL
);

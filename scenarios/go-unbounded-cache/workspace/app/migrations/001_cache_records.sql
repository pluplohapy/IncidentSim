-- Task schema for the unbounded-cache incident.
--
-- Migrations are part of the task surface: the stand applies them at boot, so
-- the service comes up with the schema the workspace declares. Each request
-- ingests a batch of records; the rendered view of a record is cached in the
-- process, and whether that cache is bounded is decided in app/handler.go.
CREATE TABLE IF NOT EXISTS cache_records (
    id         SERIAL PRIMARY KEY,
    term       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

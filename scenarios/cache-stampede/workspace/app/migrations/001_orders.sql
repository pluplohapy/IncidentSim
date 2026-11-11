-- Task schema for the cache stampede incident.
--
-- Migrations are part of the task surface: the stand applies them at boot, so
-- the service comes up with the schema the workspace declares. Kept minimal
-- and idempotent; the manifest fault only seeds sample rows.
CREATE TABLE IF NOT EXISTS orders (
    id         SERIAL PRIMARY KEY,
    customer   TEXT NOT NULL,
    total      NUMERIC NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- The summary sorts by total, so the index keeps the top-customer statement
-- cheap on a large table.
CREATE INDEX IF NOT EXISTS orders_total_idx ON orders (total DESC, id ASC);

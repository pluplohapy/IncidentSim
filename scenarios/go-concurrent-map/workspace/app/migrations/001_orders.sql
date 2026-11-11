-- Task schema for the concurrent-map incident.
--
-- Migrations are part of the task surface: the stand applies them at boot, so
-- the service comes up with the schema the workspace declares. Kept minimal
-- and idempotent; the count endpoint only reads from orders.
CREATE TABLE IF NOT EXISTS orders (
    id         SERIAL PRIMARY KEY,
    customer   TEXT NOT NULL,
    total      NUMERIC NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Task schema for the dependency-policy incident.
--
-- Migrations run at boot by the shared stand, so the service comes up with the
-- schema the workspace declares. Kept minimal and idempotent; the manifest
-- fault only seeds sample rows.
CREATE TABLE IF NOT EXISTS orders (
    id         SERIAL PRIMARY KEY,
    customer   TEXT NOT NULL,
    total      NUMERIC NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS orders_pkey ON orders (id);

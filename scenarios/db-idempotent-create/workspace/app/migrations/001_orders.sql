-- Task schema for the idempotent-create incident.
--
-- Migrations are part of the task surface: the stand applies them at boot, so
-- the service comes up with the schema the workspace declares. The table keeps
-- the client key a create is addressed by; whether that key is unique is
-- decided in app/handler.go.
CREATE TABLE IF NOT EXISTS orders (
    id         SERIAL PRIMARY KEY,
    client_key TEXT NOT NULL,
    customer   TEXT NOT NULL,
    total      NUMERIC NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

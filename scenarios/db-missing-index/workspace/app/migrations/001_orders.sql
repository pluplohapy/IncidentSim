-- Task schema for the order-lookup incident.
--
-- Migrations are part of the task surface: the stand applies them at boot, so
-- the service comes up with the schema the workspace declares. The insert is
-- guarded: the service restarts on every apply and the database is recreated
-- on a session reset, and two million rows are not something to rebuild by
-- accident.
CREATE TABLE IF NOT EXISTS orders (
    id         BIGSERIAL PRIMARY KEY,
    user_id    INTEGER NOT NULL,
    total      NUMERIC NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO orders (user_id, total)
SELECT (g % 2000) + 1, g * 1.5
FROM generate_series(1, 2000000) g
WHERE NOT EXISTS (SELECT 1 FROM orders);

ANALYZE orders;

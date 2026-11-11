-- Task schema for the transaction-lifecycle incident.
--
-- Migrations are part of the task surface: the stand applies them at boot, so
-- the service comes up with the schema the workspace declares. The statements
-- are idempotent: the service restarts on every apply and the database is
-- recreated on a session reset, so a guard keeps the row count stable.
CREATE TABLE IF NOT EXISTS orders (
    id         SERIAL PRIMARY KEY,
    customer   TEXT NOT NULL,
    total      NUMERIC NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

INSERT INTO orders (customer, total)
SELECT 'customer-' || g, g * 10
FROM generate_series(1, 1000) g
WHERE NOT EXISTS (SELECT 1 FROM orders);

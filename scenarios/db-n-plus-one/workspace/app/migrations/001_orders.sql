-- Task schema for the order-list incident.
--
-- Migrations are part of the task surface: the stand applies them at boot, so
-- the service comes up with the schema the workspace declares. The statements
-- are idempotent: the service restarts on every apply and the database is
-- recreated on a session reset, so a guard keeps the row counts stable.
CREATE TABLE IF NOT EXISTS orders (
    id         SERIAL PRIMARY KEY,
    customer   TEXT NOT NULL,
    total      NUMERIC NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS order_positions (
    id       SERIAL PRIMARY KEY,
    order_id INTEGER NOT NULL REFERENCES orders (id),
    sku      TEXT NOT NULL,
    amount   NUMERIC NOT NULL
);

CREATE INDEX IF NOT EXISTS order_positions_order_id_idx ON order_positions (order_id);

INSERT INTO orders (customer, total)
SELECT 'customer-' || g, g * 10
FROM generate_series(1, 20) g
WHERE NOT EXISTS (SELECT 1 FROM orders);

INSERT INTO order_positions (order_id, sku, amount)
SELECT o.id, 'SKU-' || o.id || '-' || g.n, g.n
FROM orders o
CROSS JOIN generate_series(1, 10) AS g(n)
WHERE NOT EXISTS (SELECT 1 FROM order_positions);

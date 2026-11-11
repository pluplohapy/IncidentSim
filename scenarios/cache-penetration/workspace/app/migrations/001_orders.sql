-- Task schema for the cache penetration incident.
--
-- An order lives across three lifecycle tables: the active table, the archive
-- and the event history. A lookup has to check all of them before it may
-- answer "not found", which is what makes an uncached absent id expensive.
CREATE TABLE IF NOT EXISTS orders (
    id         SERIAL PRIMARY KEY,
    customer   TEXT NOT NULL,
    total      NUMERIC NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS orders_archive (
    id          INTEGER PRIMARY KEY,
    customer    TEXT NOT NULL,
    total       NUMERIC NOT NULL,
    archived_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS order_events (
    id         SERIAL PRIMARY KEY,
    order_id   INTEGER NOT NULL,
    kind       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS order_events_order_id_idx ON order_events (order_id);

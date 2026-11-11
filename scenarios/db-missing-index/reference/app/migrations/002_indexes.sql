-- Supporting indexes of the order list.
--
-- The lookup by user_id filters two million rows on a column nothing indexes,
-- so every request walks the whole table. The index is what makes the lookup
-- a range scan; CONCURRENTLY keeps the table writable while it is built and
-- IF NOT EXISTS keeps the statement idempotent across restarts.
CREATE INDEX CONCURRENTLY IF NOT EXISTS orders_user_id_idx ON orders (user_id);

-- Fix pg-table-bloat: re-enable autovacuum and vacuum the table.
ALTER TABLE orders SET (autovacuum_enabled = true);
VACUUM (VERBOSE, ANALYZE) orders;

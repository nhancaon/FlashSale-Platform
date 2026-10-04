-- The order recovery job looks for PENDING orders untouched for a while (status = 'PENDING' AND updated_at < ...).
-- Almost every order is final, so this index keeps the scan to the few PENDING rows.
CREATE INDEX ix_orders_status_updated ON orders(status, updated_at);

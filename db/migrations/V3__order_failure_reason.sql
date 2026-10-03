-- V3: the order saga records why an order failed and when it was last updated.
ALTER TABLE orders ADD (failure_reason VARCHAR2(64), updated_at TIMESTAMP);

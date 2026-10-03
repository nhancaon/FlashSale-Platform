package outbox

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// Oracle is the Source backed by the outbox_events table.
type Oracle struct {
	DB *sql.DB
	// AggregatePrefix, when set, restricts the worker to aggregates whose id starts with it (used by tests so they
	// do not pick up unrelated events; empty in production).
	AggregatePrefix string
}

// claimSQL picks the oldest due NEW events, but never one that has an older NEW event of the same aggregate:
// that older event is either being sent by another worker (locked) or waiting for its retry, and sending the
// newer one first would reorder an order's events (ORDER_CONFIRMED before ORDER_CREATED).
// SKIP LOCKED hides rows other workers hold. Oracle locks rows as they are fetched, so reading exactly `limit`
// rows (with the same prefetch size) locks exactly `limit` rows.
const claimSQLFormat = `
SELECT e.id, e.aggregate_id, e.event_type, e.payload, e.attempts
FROM outbox_events e
WHERE e.status = 'NEW'
  AND (e.next_attempt_at IS NULL OR e.next_attempt_at <= CAST(SYSTIMESTAMP AS TIMESTAMP))
  AND NOT EXISTS (SELECT 1 FROM outbox_events p
                  WHERE p.aggregate_id = e.aggregate_id AND p.status = 'NEW' AND p.id < e.id)
  %s
ORDER BY e.id
FOR UPDATE SKIP LOCKED`

func (o *Oracle) Claim(ctx context.Context, limit int) (Batch, error) {
	tx, err := o.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	var rows *sql.Rows
	if o.AggregatePrefix != "" {
		rows, err = tx.QueryContext(ctx, fmt.Sprintf(claimSQLFormat, "AND e.aggregate_id LIKE :1"), o.AggregatePrefix+"%")
	} else {
		rows, err = tx.QueryContext(ctx, fmt.Sprintf(claimSQLFormat, ""))
	}
	if err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	var events []Event
	for len(events) < limit && rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.AggregateID, &e.Type, &e.Payload, &e.Attempts); err != nil {
			_ = rows.Close()
			_ = tx.Rollback()
			return nil, err
		}
		events = append(events, e)
	}
	// Close without draining: rows beyond `limit` were never fetched, so they are not locked.
	if err := rows.Close(); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if err := rows.Err(); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	if len(events) == 0 {
		_ = tx.Rollback()
		return nil, nil
	}
	return &oracleBatch{tx: tx, events: events}, nil
}

type oracleBatch struct {
	tx     *sql.Tx
	events []Event
}

func (b *oracleBatch) Events() []Event { return b.events }

func (b *oracleBatch) MarkSent(ctx context.Context, ids []int64) error {
	// One statement for the whole batch; the ids are bind variables, not concatenated values.
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = fmt.Sprintf(":%d", i+1)
		args[i] = id
	}
	_, err := b.tx.ExecContext(ctx,
		"UPDATE outbox_events SET status = 'SENT', sent_at = SYSTIMESTAMP, last_error = NULL WHERE id IN ("+
			strings.Join(placeholders, ",")+")", args...)
	return err
}

func (b *oracleBatch) MarkFailed(ctx context.Context, e Event, cause error, retryAfter time.Duration, maxAttempts int) error {
	msg := cause.Error()
	if len(msg) > 480 {
		msg = msg[:480]
	}
	status := "NEW"
	if e.Attempts+1 >= maxAttempts {
		status = "FAILED"
	}
	_, err := b.tx.ExecContext(ctx, `
		UPDATE outbox_events
		SET attempts = attempts + 1, status = :1, last_error = :2,
		    next_attempt_at = CAST(SYSTIMESTAMP AS TIMESTAMP) + NUMTODSINTERVAL(:3, 'SECOND')
		WHERE id = :4`, status, msg, int64(retryAfter.Seconds()), e.ID)
	return err
}

func (b *oracleBatch) Commit() error   { return b.tx.Commit() }
func (b *oracleBatch) Rollback() error { return b.tx.Rollback() }

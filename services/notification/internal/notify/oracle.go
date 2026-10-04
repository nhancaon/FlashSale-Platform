package notify

import (
	"context"
	"database/sql"
	"errors"

	"github.com/sijms/go-ora/v2/network"
)

const oraUniqueViolation = 1

// Oracle is the Store backed by processed_events and notifications.
type Oracle struct{ DB *sql.DB }

func (o *Oracle) Process(ctx context.Context, consumer string, eventID int64, n *Notification) (bool, error) {
	tx, err := o.DB.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	if _, err := tx.ExecContext(ctx, "INSERT INTO processed_events (event_id, consumer) VALUES (:1, :2)", eventID, consumer); err != nil {
		var oe *network.OracleError
		if errors.As(err, &oe) && oe.ErrCode == oraUniqueViolation {
			return false, nil // an earlier delivery already handled this event
		}
		return false, err
	}
	if n != nil {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO notifications (event_id, order_id, user_id, channel, message) VALUES (:1, :2, :3, :4, :5)",
			eventID, n.OrderID, n.UserID, n.Channel, n.Text); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// Package notify turns order events into notifications, exactly once per event id.
//
// Kafka delivery is at-least-once (the outbox worker may resend after a crash), so the consumer is idempotent:
// the event id is inserted into processed_events in the same transaction as the notification, and a duplicate
// insert (ORA-00001) means the event was already handled.
package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Message is a Kafka record reduced to what the handler needs.
type Message struct {
	EventID   int64 // header event-id (the outbox row id)
	EventType string
	Value     []byte
}

// Notification is what the (simulated) email/SMS gateway sends.
type Notification struct {
	OrderID string
	UserID  string
	Channel string
	Text    string
}

// Store persists the inbox row and the notification atomically.
type Store interface {
	// Process records eventID for consumer and, when n is not nil, the notification, in one transaction.
	// It returns false (and does nothing) when the event was already processed.
	Process(ctx context.Context, consumer string, eventID int64, n *Notification) (bool, error)
}

// Result is the outcome for one message.
type Result string

const (
	Sent      Result = "sent"      // a notification was created
	Skipped   Result = "skipped"   // processed, nothing to send (for example ORDER_CREATED)
	Duplicate Result = "duplicate" // already processed earlier
	Poison    Result = "poison"    // unreadable payload, recorded and skipped
)

type orderEvent struct {
	OrderID       string `json:"orderId"`
	UserID        string `json:"userId"`
	Status        string `json:"status"`
	FailureReason string `json:"failureReason"`
}

// Handler processes messages for one consumer name.
type Handler struct {
	Consumer string
	Store    Store
}

// Handle returns an error only for infrastructure trouble (database down): the caller must retry the message.
func (h *Handler) Handle(ctx context.Context, m Message) (Result, error) {
	if m.EventID <= 0 {
		return Poison, errors.New("message has no valid event-id header") // cannot dedupe: surface to the caller
	}
	var ev orderEvent
	if err := json.Unmarshal(m.Value, &ev); err != nil || ev.OrderID == "" {
		// A payload that will never parse must not block the partition: record it and move on.
		processed, perr := h.Store.Process(ctx, h.Consumer, m.EventID, nil)
		if perr != nil {
			return "", perr
		}
		if !processed {
			return Duplicate, nil
		}
		return Poison, nil
	}

	n := build(m.EventType, ev)
	processed, err := h.Store.Process(ctx, h.Consumer, m.EventID, n)
	if err != nil {
		return "", err
	}
	switch {
	case !processed:
		return Duplicate, nil
	case n == nil:
		return Skipped, nil
	default:
		return Sent, nil
	}
}

func build(eventType string, ev orderEvent) *Notification {
	switch eventType {
	case "ORDER_CONFIRMED":
		return &Notification{ev.OrderID, ev.UserID, "EMAIL", fmt.Sprintf("Your order %s is confirmed. Thank you!", ev.OrderID)}
	case "ORDER_FAILED":
		return &Notification{ev.OrderID, ev.UserID, "SMS", fmt.Sprintf("Your order %s could not be completed (%s).", ev.OrderID, ev.FailureReason)}
	default:
		return nil // ORDER_CREATED and unknown types: nothing to tell the customer
	}
}

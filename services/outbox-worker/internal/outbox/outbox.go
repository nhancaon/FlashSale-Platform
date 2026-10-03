// Package outbox moves rows of the outbox_events table to Kafka (Transactional Outbox, relay side).
//
// Several workers (goroutines, or processes) compete for rows with SELECT ... FOR UPDATE SKIP LOCKED: a row
// locked by one worker is invisible to the others, so no event is published twice by two live workers. Delivery
// is at-least-once overall: if a worker dies after publishing but before its commit, the rows become visible again
// and are sent again, so consumers must be idempotent (the notification service is).
package outbox

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

// Event is one row of outbox_events.
type Event struct {
	ID          int64
	AggregateID string
	Type        string
	Payload     string
	Attempts    int
}

// Batch is a set of claimed events held under row locks inside one transaction.
type Batch interface {
	Events() []Event
	// MarkSent records successful delivery.
	MarkSent(ctx context.Context, ids []int64) error
	// MarkFailed counts a failed attempt. When attempts reach the limit the event becomes FAILED, otherwise it is
	// retried after the backoff.
	MarkFailed(ctx context.Context, e Event, cause error, retryAfter time.Duration, maxAttempts int) error
	Commit() error
	Rollback() error
}

// Source claims batches.
type Source interface {
	// Claim locks up to limit unsent events that are due. It returns nil, nil when there is nothing to do.
	Claim(ctx context.Context, limit int) (Batch, error)
}

// Publisher delivers events to the broker. The returned slice has one entry per event: nil means delivered.
type Publisher interface {
	Publish(ctx context.Context, events []Event) []error
}

// Metrics are the counters the processor updates (nil-safe through the no-op default).
type Metrics interface {
	Published(n int)
	Failed(n int) // attempts that failed (will be retried or given up)
	GaveUp(n int) // events moved to FAILED
	BatchDuration(d time.Duration)
}

type noopMetrics struct{}

func (noopMetrics) Published(int)               {}
func (noopMetrics) Failed(int)                  {}
func (noopMetrics) GaveUp(int)                  {}
func (noopMetrics) BatchDuration(time.Duration) {}

// Processor runs claim, publish, mark, commit as one unit of work.
type Processor struct {
	Source      Source
	Publisher   Publisher
	BatchSize   int
	MaxAttempts int
	Metrics     Metrics
	Log         *slog.Logger
	// Backoff returns the delay before attempt number `attempts` is retried.
	Backoff func(attempts int) time.Duration
}

func (p *Processor) metrics() Metrics {
	if p.Metrics == nil {
		return noopMetrics{}
	}
	return p.Metrics
}

// ProcessOnce handles one batch and returns how many events it claimed. An error means the broker (or the
// database) is having trouble: the caller should back off before the next call.
func (p *Processor) ProcessOnce(ctx context.Context) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err() // shutting down: do not start new work
	}
	// Once a batch is claimed it must be finished even if shutdown starts: cancelling would roll the transaction
	// back after Kafka already accepted the messages, and they would be sent again. So the work runs on a context
	// that ignores cancellation of ctx, bounded by a timeout instead.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	start := time.Now()
	batch, err := p.Source.Claim(ctx, p.BatchSize)
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}
	if batch == nil {
		return 0, nil
	}
	events := batch.Events()
	if len(events) == 0 {
		_ = batch.Rollback()
		return 0, nil
	}

	results := p.Publisher.Publish(ctx, events)
	var sent []int64
	var firstErr error
	for i, e := range events {
		if results[i] == nil {
			sent = append(sent, e.ID)
			continue
		}
		if firstErr == nil {
			firstErr = results[i]
		}
		p.metrics().Failed(1)
		if e.Attempts+1 >= p.MaxAttempts {
			p.metrics().GaveUp(1)
			p.Log.Error("outbox event gave up", "event_id", e.ID, "type", e.Type, "attempts", e.Attempts+1, "error", results[i])
		}
		if err := batch.MarkFailed(ctx, e, results[i], p.Backoff(e.Attempts+1), p.MaxAttempts); err != nil {
			_ = batch.Rollback()
			return 0, fmt.Errorf("mark failed: %w", err)
		}
	}
	if len(sent) > 0 {
		if err := batch.MarkSent(ctx, sent); err != nil {
			_ = batch.Rollback()
			return 0, fmt.Errorf("mark sent: %w", err)
		}
	}
	if err := batch.Commit(); err != nil {
		// Published but not recorded: these events will be sent again (at-least-once).
		return 0, fmt.Errorf("commit: %w", err)
	}
	p.metrics().Published(len(sent))
	p.metrics().BatchDuration(time.Since(start))
	return len(events), firstErr
}

// ExponentialBackoff doubles from 1 s up to max.
func ExponentialBackoff(max time.Duration) func(int) time.Duration {
	return func(attempts int) time.Duration {
		d := time.Second << min(attempts-1, 10)
		if d > max {
			return max
		}
		return d
	}
}

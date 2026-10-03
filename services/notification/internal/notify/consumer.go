package notify

import (
	"context"
	"log/slog"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"
)

// Fetcher is the part of kafka.Reader the loop uses (so it can be faked in tests).
type Fetcher interface {
	FetchMessage(ctx context.Context) (kafka.Message, error)
	CommitMessages(ctx context.Context, msgs ...kafka.Message) error
}

// Run consumes until ctx is cancelled. Offsets are committed only after the message was handled, so a crash
// re-delivers the message (the handler is idempotent). Messages of one partition are handled in order.
func Run(ctx context.Context, f Fetcher, h *Handler, onResult func(Result), log *slog.Logger) {
	for ctx.Err() == nil {
		m, err := f.FetchMessage(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			log.Warn("fetch failed", "error", err)
			sleep(ctx, time.Second)
			continue
		}
		msg := toMessage(m)
		var res Result
		for attempt := 1; ; attempt++ { // retry infrastructure errors; never skip a message silently
			res, err = h.Handle(ctx, msg)
			if err == nil || ctx.Err() != nil {
				break
			}
			log.Error("handling failed, retrying", "event_id", msg.EventID, "attempt", attempt, "error", err)
			sleep(ctx, time.Duration(min(attempt, 10))*time.Second)
		}
		if err != nil {
			return // shutting down mid-retry: leave the offset uncommitted, the message comes back
		}
		onResult(res)
		if err := f.CommitMessages(context.WithoutCancel(ctx), m); err != nil {
			log.Warn("commit offset failed (the message may be delivered again)", "error", err)
		}
	}
}

func toMessage(m kafka.Message) Message {
	out := Message{Value: m.Value}
	for _, h := range m.Headers {
		switch h.Key {
		case "event-id":
			out.EventID, _ = strconv.ParseInt(string(h.Value), 10, 64)
		case "event-type":
			out.EventType = string(h.Value)
		}
	}
	return out
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

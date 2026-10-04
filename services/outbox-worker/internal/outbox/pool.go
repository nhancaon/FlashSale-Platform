package outbox

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"time"
)

// Pool runs N competing workers (Producer-Consumer without a queue: the database is the queue).
type Pool struct {
	Workers      int
	Processor    *Processor
	PollInterval time.Duration // sleep when the table has nothing due
	MaxBackoff   time.Duration // cap for the sleep after errors
	Log          *slog.Logger
}

// Run starts the workers and blocks until ctx is cancelled and every worker has finished its current batch.
func (p *Pool) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 1; i <= p.Workers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			p.loop(ctx, id)
		}(i)
	}
	wg.Wait()
}

func (p *Pool) loop(ctx context.Context, id int) {
	failures := 0
	for ctx.Err() == nil {
		n, err := p.Processor.ProcessOnce(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			failures++
			wait := min(p.PollInterval<<min(failures, 8), p.MaxBackoff)
			wait += time.Duration(rand.Int64N(int64(wait/2) + 1)) //nolint:gosec // jitter so workers do not retry in lockstep
			p.Log.Warn("outbox batch failed, backing off", "worker", id, "error", err, "wait", wait)
			sleep(ctx, wait)
		case n == 0:
			failures = 0
			sleep(ctx, p.PollInterval)
		default:
			failures = 0 // a full batch: go straight back for more
		}
	}
	p.Log.Info("outbox worker stopped", "worker", id)
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

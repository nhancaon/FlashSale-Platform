package outbox_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nhancaon/flashsale/services/outbox-worker/internal/outbox"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type failure struct {
	id          int64
	retryAfter  time.Duration
	maxAttempts int
}

type fakeBatch struct {
	events      []outbox.Event
	sent        []int64
	failed      []failure
	committed   bool
	rolledBack  bool
	markSentErr error
}

func (b *fakeBatch) Events() []outbox.Event { return b.events }
func (b *fakeBatch) MarkSent(_ context.Context, ids []int64) error {
	b.sent = append(b.sent, ids...)
	return b.markSentErr
}
func (b *fakeBatch) MarkFailed(_ context.Context, e outbox.Event, _ error, retryAfter time.Duration, maxAttempts int) error {
	b.failed = append(b.failed, failure{e.ID, retryAfter, maxAttempts})
	return nil
}
func (b *fakeBatch) Commit() error   { b.committed = true; return nil }
func (b *fakeBatch) Rollback() error { b.rolledBack = true; return nil }

type fakeSource struct {
	mu      sync.Mutex
	batches []*fakeBatch
	err     error
	claims  atomic.Int64
}

func (s *fakeSource) Claim(context.Context, int) (outbox.Batch, error) {
	s.claims.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	if len(s.batches) == 0 {
		return nil, nil
	}
	b := s.batches[0]
	s.batches = s.batches[1:]
	return b, nil
}

type fakePublisher struct {
	fail      map[int64]error
	onPublish func()
	published []int64
	mu        sync.Mutex
}

func (p *fakePublisher) Publish(_ context.Context, events []outbox.Event) []error {
	if p.onPublish != nil {
		p.onPublish()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	res := make([]error, len(events))
	for i, e := range events {
		if err := p.fail[e.ID]; err != nil {
			res[i] = err
			continue
		}
		p.published = append(p.published, e.ID)
	}
	return res
}

type countingMetrics struct{ published, failed, gaveUp atomic.Int64 }

func (m *countingMetrics) Published(n int)             { m.published.Add(int64(n)) }
func (m *countingMetrics) Failed(n int)                { m.failed.Add(int64(n)) }
func (m *countingMetrics) GaveUp(n int)                { m.gaveUp.Add(int64(n)) }
func (m *countingMetrics) BatchDuration(time.Duration) {}

func events(ids ...int64) []outbox.Event {
	out := make([]outbox.Event, len(ids))
	for i, id := range ids {
		out[i] = outbox.Event{ID: id, AggregateID: "agg", Type: "ORDER_CREATED", Payload: "{}"}
	}
	return out
}

func processor(src outbox.Source, pub outbox.Publisher, m outbox.Metrics) *outbox.Processor {
	return &outbox.Processor{Source: src, Publisher: pub, BatchSize: 10, MaxAttempts: 3, Metrics: m, Log: quiet,
		Backoff: func(a int) time.Duration { return time.Duration(a) * time.Second }}
}

func TestSuccessfulBatchIsMarkedSentAndCommitted(t *testing.T) {
	b := &fakeBatch{events: events(1, 2, 3)}
	pub := &fakePublisher{}
	m := &countingMetrics{}
	n, err := processor(&fakeSource{batches: []*fakeBatch{b}}, pub, m).ProcessOnce(context.Background())

	require.NoError(t, err)
	assert.Equal(t, 3, n)
	assert.Equal(t, []int64{1, 2, 3}, b.sent)
	assert.True(t, b.committed)
	assert.Empty(t, b.failed)
	assert.EqualValues(t, 3, m.published.Load())
}

func TestNothingToDo(t *testing.T) {
	pub := &fakePublisher{}
	n, err := processor(&fakeSource{}, pub, nil).ProcessOnce(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n)
	assert.Empty(t, pub.published)
}

func TestFailedEventsAreCountedAndRetriedLaterWhileOthersAreSent(t *testing.T) {
	b := &fakeBatch{events: []outbox.Event{{ID: 1}, {ID: 2, Attempts: 1}, {ID: 3}}}
	pub := &fakePublisher{fail: map[int64]error{2: errors.New("broker said no")}}
	m := &countingMetrics{}

	n, err := processor(&fakeSource{batches: []*fakeBatch{b}}, pub, m).ProcessOnce(context.Background())

	assert.ErrorContains(t, err, "broker said no", "the caller is told so it can back off")
	assert.Equal(t, 3, n)
	assert.Equal(t, []int64{1, 3}, b.sent)
	require.Len(t, b.failed, 1)
	assert.EqualValues(t, 2, b.failed[0].id)
	assert.Equal(t, 2*time.Second, b.failed[0].retryAfter, "backoff grows with the attempt number")
	assert.Equal(t, 3, b.failed[0].maxAttempts)
	assert.True(t, b.committed, "failure bookkeeping must be committed")
	assert.EqualValues(t, 0, m.gaveUp.Load())
}

func TestEventGivesUpAtTheAttemptLimit(t *testing.T) {
	b := &fakeBatch{events: []outbox.Event{{ID: 7, Attempts: 2}}} // this is attempt 3 of 3
	pub := &fakePublisher{fail: map[int64]error{7: errors.New("too large")}}
	m := &countingMetrics{}

	_, err := processor(&fakeSource{batches: []*fakeBatch{b}}, pub, m).ProcessOnce(context.Background())

	assert.Error(t, err)
	assert.EqualValues(t, 1, m.gaveUp.Load())
	assert.Equal(t, 3, b.failed[0].maxAttempts, "the store turns the row into FAILED when attempts reach the limit")
}

func TestClaimErrorIsReported(t *testing.T) {
	_, err := processor(&fakeSource{err: errors.New("db down")}, &fakePublisher{}, nil).ProcessOnce(context.Background())
	assert.ErrorContains(t, err, "db down")
}

func TestBookkeepingFailureRollsBack(t *testing.T) {
	b := &fakeBatch{events: events(1), markSentErr: errors.New("lost connection")}
	_, err := processor(&fakeSource{batches: []*fakeBatch{b}}, &fakePublisher{}, nil).ProcessOnce(context.Background())
	assert.ErrorContains(t, err, "lost connection")
	assert.True(t, b.rolledBack)
	assert.False(t, b.committed)
}

func TestAlreadyCancelledContextStartsNoWork(t *testing.T) {
	src := &fakeSource{batches: []*fakeBatch{{events: events(1)}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := processor(src, &fakePublisher{}, nil).ProcessOnce(ctx)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Zero(t, src.claims.Load(), "shutting down: do not claim new events")
}

// Shutdown during a batch must not abort it: cancelling would roll the transaction back after Kafka already
// accepted the messages, and they would be published a second time.
func TestShutdownDuringABatchStillFinishesIt(t *testing.T) {
	b := &fakeBatch{events: events(1, 2)}
	ctx, cancel := context.WithCancel(context.Background())
	pub := &fakePublisher{onPublish: cancel} // SIGTERM arrives while publishing

	n, err := processor(&fakeSource{batches: []*fakeBatch{b}}, pub, nil).ProcessOnce(ctx)

	require.NoError(t, err)
	assert.Equal(t, 2, n)
	assert.True(t, b.committed)
	assert.Equal(t, []int64{1, 2}, b.sent)
}

func TestPoolRunsAllWorkersAndStopsOnCancel(t *testing.T) {
	var batches []*fakeBatch
	for i := 0; i < 30; i++ {
		batches = append(batches, &fakeBatch{events: events(int64(i + 1))})
	}
	src := &fakeSource{batches: batches}
	pub := &fakePublisher{}
	ctx, cancel := context.WithCancel(context.Background())
	pool := &outbox.Pool{Workers: 3, Processor: processor(src, pub, nil), PollInterval: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond, Log: quiet}

	done := make(chan struct{})
	go func() { pool.Run(ctx); close(done) }()
	require.Eventually(t, func() bool { pub.mu.Lock(); defer pub.mu.Unlock(); return len(pub.published) == 30 }, 3*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("pool did not stop after cancel")
	}
	for _, b := range batches {
		assert.True(t, b.committed)
	}
}

func TestPoolBacksOffOnErrorsAndRecovers(t *testing.T) {
	src := &fakeSource{err: errors.New("broker down")}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool := &outbox.Pool{Workers: 1, Processor: processor(src, &fakePublisher{}, nil), PollInterval: 10 * time.Millisecond, MaxBackoff: 80 * time.Millisecond, Log: quiet}
	go pool.Run(ctx)

	time.Sleep(300 * time.Millisecond)
	calls := src.claims.Load()
	assert.Less(t, calls, int64(15), "failing claims must back off, not spin (got %d in 300ms)", calls)

	src.mu.Lock()
	src.err = nil
	src.batches = []*fakeBatch{{events: events(1)}}
	src.mu.Unlock()
	require.Eventually(t, func() bool { src.mu.Lock(); defer src.mu.Unlock(); return len(src.batches) == 0 }, 2*time.Second, 10*time.Millisecond)
}

func TestExponentialBackoff(t *testing.T) {
	b := outbox.ExponentialBackoff(30 * time.Second)
	assert.Equal(t, time.Second, b(1))
	assert.Equal(t, 2*time.Second, b(2))
	assert.Equal(t, 4*time.Second, b(3))
	assert.Equal(t, 30*time.Second, b(20), "capped")
}

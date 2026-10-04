package notify_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nhancaon/flashsale/services/notification/internal/notify"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// memStore is an in-memory Store with the same contract as the Oracle one.
type memStore struct {
	mu        sync.Mutex
	processed map[int64]bool
	sent      []notify.Notification
	failNext  int // number of calls that fail with an infrastructure error
}

func newMemStore() *memStore { return &memStore{processed: map[int64]bool{}} }

func (s *memStore) Process(_ context.Context, _ string, id int64, n *notify.Notification) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext > 0 {
		s.failNext--
		return false, errors.New("database down")
	}
	if s.processed[id] {
		return false, nil
	}
	s.processed[id] = true
	if n != nil {
		s.sent = append(s.sent, *n)
	}
	return true, nil
}

func msg(id int64, typ, payload string) notify.Message {
	return notify.Message{EventID: id, EventType: typ, Value: []byte(payload)}
}

const confirmed = `{"orderId":"o-1","userId":"u-1","status":"CONFIRMED"}`

func TestConfirmedOrderSendsAnEmail(t *testing.T) {
	st := newMemStore()
	h := &notify.Handler{Consumer: "c", Store: st}
	res, err := h.Handle(context.Background(), msg(1, "ORDER_CONFIRMED", confirmed))
	require.NoError(t, err)
	assert.Equal(t, notify.Sent, res)
	require.Len(t, st.sent, 1)
	assert.Equal(t, "EMAIL", st.sent[0].Channel)
	assert.Contains(t, st.sent[0].Text, "o-1")
}

func TestFailedOrderSendsAnSMSWithTheReason(t *testing.T) {
	st := newMemStore()
	h := &notify.Handler{Consumer: "c", Store: st}
	res, err := h.Handle(context.Background(), msg(2, "ORDER_FAILED", `{"orderId":"o-2","userId":"u-2","failureReason":"OUT_OF_STOCK"}`))
	require.NoError(t, err)
	assert.Equal(t, notify.Sent, res)
	assert.Equal(t, "SMS", st.sent[0].Channel)
	assert.Contains(t, st.sent[0].Text, "OUT_OF_STOCK")
}

func TestOrderCreatedIsRecordedButNotNotified(t *testing.T) {
	st := newMemStore()
	h := &notify.Handler{Consumer: "c", Store: st}
	res, err := h.Handle(context.Background(), msg(3, "ORDER_CREATED", confirmed))
	require.NoError(t, err)
	assert.Equal(t, notify.Skipped, res)
	assert.Empty(t, st.sent)
	assert.True(t, st.processed[3], "still recorded, so a redelivery is recognised")
}

func TestRedeliveryOfTheSameEventSendsOnce(t *testing.T) {
	st := newMemStore()
	h := &notify.Handler{Consumer: "c", Store: st}
	first, _ := h.Handle(context.Background(), msg(4, "ORDER_CONFIRMED", confirmed))
	second, err := h.Handle(context.Background(), msg(4, "ORDER_CONFIRMED", confirmed))
	require.NoError(t, err)
	assert.Equal(t, notify.Sent, first)
	assert.Equal(t, notify.Duplicate, second)
	assert.Len(t, st.sent, 1)
}

func TestUnreadablePayloadIsRecordedAndSkipped(t *testing.T) {
	st := newMemStore()
	h := &notify.Handler{Consumer: "c", Store: st}
	res, err := h.Handle(context.Background(), msg(5, "ORDER_CONFIRMED", `not json`))
	require.NoError(t, err, "a poison message must not block the partition")
	assert.Equal(t, notify.Poison, res)
	assert.True(t, st.processed[5])
	res, _ = h.Handle(context.Background(), msg(5, "ORDER_CONFIRMED", `not json`))
	assert.Equal(t, notify.Duplicate, res)
}

func TestMissingEventIDIsAnError(t *testing.T) {
	h := &notify.Handler{Consumer: "c", Store: newMemStore()}
	_, err := h.Handle(context.Background(), msg(0, "ORDER_CONFIRMED", confirmed))
	assert.Error(t, err)
}

func TestInfrastructureErrorsPropagate(t *testing.T) {
	st := newMemStore()
	st.failNext = 1
	h := &notify.Handler{Consumer: "c", Store: st}
	_, err := h.Handle(context.Background(), msg(6, "ORDER_CONFIRMED", confirmed))
	assert.ErrorContains(t, err, "database down")
	assert.False(t, st.processed[6], "nothing was recorded, so the message can be retried")
}

// ---- consumer loop ----

type fakeFetcher struct {
	mu        sync.Mutex
	queue     []kafka.Message
	committed []int64
}

func (f *fakeFetcher) FetchMessage(ctx context.Context) (kafka.Message, error) {
	f.mu.Lock()
	if len(f.queue) > 0 {
		m := f.queue[0]
		f.queue = f.queue[1:]
		f.mu.Unlock()
		return m, nil
	}
	f.mu.Unlock()
	<-ctx.Done()
	return kafka.Message{}, ctx.Err()
}

func (f *fakeFetcher) CommitMessages(_ context.Context, msgs ...kafka.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range msgs {
		f.committed = append(f.committed, m.Offset)
	}
	return nil
}

func kmsg(offset int64, eventID int64, typ, payload string) kafka.Message {
	return kafka.Message{Offset: offset, Value: []byte(payload), Headers: []kafka.Header{
		{Key: "event-id", Value: []byte(strconv.FormatInt(eventID, 10))}, {Key: "event-type", Value: []byte(typ)}}}
}

func runLoop(t *testing.T, f *fakeFetcher, st *memStore) (counts map[notify.Result]int, stop func()) {
	t.Helper()
	counts = map[notify.Result]int{}
	var mu sync.Mutex
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		notify.Run(ctx, f, &notify.Handler{Consumer: "c", Store: st}, func(r notify.Result) { mu.Lock(); counts[r]++; mu.Unlock() }, quiet)
		close(done)
	}()
	return counts, func() {
		cancel()
		<-done
	}
}

func TestLoopCommitsOffsetsAfterHandlingAndDedupesRedeliveries(t *testing.T) {
	f := &fakeFetcher{queue: []kafka.Message{
		kmsg(0, 10, "ORDER_CREATED", confirmed),
		kmsg(1, 11, "ORDER_CONFIRMED", confirmed),
		kmsg(2, 11, "ORDER_CONFIRMED", confirmed), // the outbox published event 11 twice (a crash)
	}}
	st := newMemStore()
	_, stop := runLoop(t, f, st)
	require.Eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.committed) == 3 }, 2*time.Second, 10*time.Millisecond)
	stop()

	assert.Equal(t, []int64{0, 1, 2}, f.committed)
	assert.Len(t, st.sent, 1, "one notification for event 11 despite two deliveries")
}

func TestLoopRetriesInfrastructureErrorsWithoutSkippingTheMessage(t *testing.T) {
	f := &fakeFetcher{queue: []kafka.Message{kmsg(0, 20, "ORDER_CONFIRMED", confirmed)}}
	st := newMemStore()
	st.failNext = 1
	_, stop := runLoop(t, f, st)
	require.Eventually(t, func() bool { f.mu.Lock(); defer f.mu.Unlock(); return len(f.committed) == 1 }, 5*time.Second, 20*time.Millisecond)
	stop()
	assert.Len(t, st.sent, 1, "handled after the database came back")
}

func TestLoopDoesNotCommitAnUnhandledMessageOnShutdown(t *testing.T) {
	f := &fakeFetcher{queue: []kafka.Message{kmsg(0, 30, "ORDER_CONFIRMED", confirmed)}}
	st := newMemStore()
	st.failNext = 1000 // database permanently down
	_, stop := runLoop(t, f, st)
	time.Sleep(200 * time.Millisecond)
	stop()
	assert.Empty(t, f.committed, "an offset is only committed after the message was handled")
}

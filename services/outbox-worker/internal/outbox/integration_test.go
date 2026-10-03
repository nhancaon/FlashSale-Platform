package outbox_test

// Integration tests against the real Oracle and Kafka of `make up` (+ `make db-migrate`). They need
// DB_PASSWORD (and optionally DB_HOST, KAFKA_BROKERS) and skip with a message when the environment is missing.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/segmentio/kafka-go"
	_ "github.com/sijms/go-ora/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nhancaon/flashsale/services/outbox-worker/internal/outbox"
	"github.com/nhancaon/flashsale/services/outbox-worker/internal/publisher"
)

const batchSize = 10

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	pw := os.Getenv("DB_PASSWORD")
	if pw == "" {
		t.Skip("DB_PASSWORD not set: run `make up && make db-migrate` and use make test-go")
	}
	dsn := fmt.Sprintf("oracle://%s:%s@%s:%s/%s?PREFETCH_ROWS=%d", url.QueryEscape(getenv("DB_USER", "flashsale")),
		url.QueryEscape(pw), getenv("DB_HOST", "localhost"), getenv("DB_PORT", "1521"), getenv("DB_SERVICE", "FREEPDB1"), batchSize)
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(12)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func brokers() []string { return []string{getenv("KAFKA_BROKERS", "localhost:29092")} }

// seed inserts perAggregate events (E1, E2, ...) for each of n aggregates, one aggregate at a time so ids of one
// aggregate are increasing. It returns the aggregate prefix and the inserted ids.
func seed(t *testing.T, db *sql.DB, n, perAggregate int) (string, []int64) {
	t.Helper()
	prefix := fmt.Sprintf("it-%d-", time.Now().UnixNano())
	var ids []int64
	for a := 0; a < n; a++ {
		for e := 1; e <= perAggregate; e++ {
			var id int64
			_, err := db.Exec("INSERT INTO outbox_events (aggregate_id, event_type, payload) VALUES (:1, :2, :3)",
				fmt.Sprintf("%s%d", prefix, a), fmt.Sprintf("E%d", e), fmt.Sprintf(`{"orderId":"%s%d","seq":%d}`, prefix, a, e))
			require.NoError(t, err)
			require.NoError(t, db.QueryRow("SELECT MAX(id) FROM outbox_events WHERE aggregate_id = :1", fmt.Sprintf("%s%d", prefix, a)).Scan(&id))
			ids = append(ids, id)
		}
	}
	return prefix, ids
}

func countByStatus(t *testing.T, db *sql.DB, prefix, status string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM outbox_events WHERE aggregate_id LIKE :1 AND status = :2", prefix+"%", status).Scan(&n))
	return n
}

type received struct {
	eventID   int64
	aggregate string
	eventType string
	partition int
	offset    int64
}

// readAll reads every partition of the topic until `want` messages arrived or the timeout passes.
func readAll(t *testing.T, topic string, partitions, want int, wait time.Duration) []received {
	t.Helper()
	var mu sync.Mutex
	var out []received
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	var wg sync.WaitGroup
	for p := 0; p < partitions; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			r := kafka.NewReader(kafka.ReaderConfig{Brokers: brokers(), Topic: topic, Partition: p, MinBytes: 1, MaxBytes: 1 << 20, MaxWait: 200 * time.Millisecond, StartOffset: kafka.FirstOffset})
			defer r.Close()
			for {
				m, err := r.ReadMessage(ctx)
				if err != nil {
					return
				}
				rec := received{aggregate: string(m.Key), partition: p, offset: m.Offset}
				for _, h := range m.Headers {
					switch h.Key {
					case "event-id":
						rec.eventID, _ = strconv.ParseInt(string(h.Value), 10, 64)
					case "event-type":
						rec.eventType = string(h.Value)
					}
				}
				mu.Lock()
				out = append(out, rec)
				done := len(out) >= want
				mu.Unlock()
				if done {
					cancel()
					return
				}
			}
		}(p)
	}
	wg.Wait()
	return out
}

func newTopic(t *testing.T, partitions int) string {
	t.Helper()
	topic := fmt.Sprintf("it.outbox.%d", time.Now().UnixNano())
	require.NoError(t, publisher.EnsureTopic(context.Background(), brokers()[0], topic, partitions))
	return topic
}

func pool(db *sql.DB, prefix string, pub outbox.Publisher, workers int, src outbox.Source) (*outbox.Pool, *countingMetrics) {
	return poolWith(db, prefix, pub, workers, src, 100, 50*time.Millisecond)
}

// poolWith sets the retry policy: generous by default so transient broker errors never exhaust the attempts.
func poolWith(db *sql.DB, prefix string, pub outbox.Publisher, workers int, src outbox.Source, maxAttempts int, backoff time.Duration) (*outbox.Pool, *countingMetrics) {
	if src == nil {
		src = &outbox.Oracle{DB: db, AggregatePrefix: prefix}
	}
	m := &countingMetrics{}
	return &outbox.Pool{
		Workers: workers, PollInterval: 20 * time.Millisecond, MaxBackoff: 200 * time.Millisecond, Log: quiet,
		Processor: &outbox.Processor{Source: src, Publisher: pub, BatchSize: batchSize, MaxAttempts: maxAttempts, Metrics: m, Log: quiet,
			Backoff: func(int) time.Duration { return backoff }},
	}, m
}

// Two workers claiming at the same time must get disjoint rows (SKIP LOCKED), and each locks only about a batch.
func TestConcurrentClaimsAreDisjoint(t *testing.T) {
	db := openDB(t)
	prefix, _ := seed(t, db, 100, 1)
	src := &outbox.Oracle{DB: db, AggregatePrefix: prefix}

	a, err := src.Claim(context.Background(), batchSize)
	require.NoError(t, err)
	b, err := src.Claim(context.Background(), batchSize) // a still holds its locks
	require.NoError(t, err)
	require.NotNil(t, a)
	require.NotNil(t, b)
	defer a.Rollback()
	defer b.Rollback()

	ids := map[int64]bool{}
	for _, e := range append(a.Events(), b.Events()...) {
		assert.False(t, ids[e.ID], "event %d claimed twice", e.ID)
		ids[e.ID] = true
	}
	assert.Len(t, a.Events(), batchSize)
	assert.Len(t, b.Events(), batchSize, "the second worker is not starved by the first one's locks")
}

// An event is never claimed while an older NEW event of the same aggregate exists: that one may be in flight in
// another worker, and sending the newer one first would reorder the aggregate's events.
func TestNewerEventWaitsForTheOlderOneOfTheSameAggregate(t *testing.T) {
	db := openDB(t)
	prefix, _ := seed(t, db, 1, 2) // E1, E2 of one aggregate
	src := &outbox.Oracle{DB: db, AggregatePrefix: prefix}

	first, err := src.Claim(context.Background(), 1)
	require.NoError(t, err)
	require.NotNil(t, first)
	defer first.Rollback()
	assert.Equal(t, "E1", first.Events()[0].Type)

	second, err := src.Claim(context.Background(), 10)
	require.NoError(t, err)
	assert.Nil(t, second, "E2 must wait while E1 is in flight")
}

// The acceptance test of the phase: 3 workers, every event is published exactly once, in order per aggregate.
func TestThreeWorkersPublishEveryEventExactlyOnceInOrder(t *testing.T) {
	db := openDB(t)
	const aggregates, perAggregate = 60, 3
	prefix, ids := seed(t, db, aggregates, perAggregate)
	topic := newTopic(t, 3)
	kp := publisher.NewKafka(brokers(), topic)
	defer kp.Close()
	p, m := pool(db, prefix, kp, 3, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	require.Eventually(t, func() bool { return countByStatus(t, db, prefix, "SENT") == len(ids) }, 60*time.Second, 100*time.Millisecond)
	cancel()
	<-done

	got := readAll(t, topic, 3, len(ids)+1, 8*time.Second) // +1: wait for a duplicate that must not come
	require.Len(t, got, len(ids), "every event exactly once, no duplicates")
	seen := map[int64]int{}
	for _, r := range got {
		seen[r.eventID]++
	}
	for _, id := range ids {
		assert.Equal(t, 1, seen[id], "event %d", id)
	}
	assert.EqualValues(t, len(ids), m.published.Load())

	// Order per aggregate: same partition (key hash) and increasing offsets with E1 < E2 < E3.
	byAgg := map[string][]received{}
	for _, r := range got {
		byAgg[r.aggregate] = append(byAgg[r.aggregate], r)
	}
	for agg, recs := range byAgg {
		sort.Slice(recs, func(i, j int) bool { return recs[i].offset < recs[j].offset })
		require.Len(t, recs, perAggregate, agg)
		for i, r := range recs {
			assert.Equal(t, recs[0].partition, r.partition, "one aggregate, one partition")
			assert.Equal(t, fmt.Sprintf("E%d", i+1), r.eventType, "order of %s", agg)
		}
	}
}

var simulatedCrashes atomic.Int64

type crashingBatch struct {
	outbox.Batch
	crash bool
}

// Commit "crashes" (the transaction rolls back) after Kafka already took the messages: the process died between
// publish and commit.
func (b crashingBatch) Commit() error {
	if b.crash {
		_ = b.Batch.Rollback()
		simulatedCrashes.Add(1)
		return errors.New("simulated crash between publish and commit")
	}
	return b.Batch.Commit()
}

type crashingSource struct {
	outbox.Source
	mu      sync.Mutex
	crashes int
}

func (s *crashingSource) Claim(ctx context.Context, limit int) (outbox.Batch, error) {
	b, err := s.Source.Claim(ctx, limit)
	if b == nil || err != nil {
		return b, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.crashes > 0 {
		s.crashes--
		return crashingBatch{b, true}, nil
	}
	return crashingBatch{b, false}, nil
}

// A worker dying mid-flight must not lose events: the rows become visible again and are sent (at least once).
func TestWorkerCrashAfterPublishLosesNothing(t *testing.T) {
	db := openDB(t)
	prefix, ids := seed(t, db, 30, 2)
	topic := newTopic(t, 3)
	kp := publisher.NewKafka(brokers(), topic)
	defer kp.Close()
	src := &crashingSource{Source: &outbox.Oracle{DB: db, AggregatePrefix: prefix}, crashes: 4}
	p, _ := pool(db, prefix, kp, 3, src)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	require.Eventually(t, func() bool {
		t.Logf("SENT=%d NEW=%d FAILED=%d", countByStatus(t, db, prefix, "SENT"), countByStatus(t, db, prefix, "NEW"), countByStatus(t, db, prefix, "FAILED"))
		return countByStatus(t, db, prefix, "SENT") == len(ids)
	}, 60*time.Second, 2*time.Second)
	cancel()
	<-done

	got := readAll(t, topic, 3, len(ids)+200, 8*time.Second)
	unique := map[int64]int{}
	for _, r := range got {
		unique[r.eventID]++
	}
	for _, id := range ids {
		assert.GreaterOrEqual(t, unique[id], 1, "event %d was lost", id)
	}
	duplicates := len(got) - len(unique)
	t.Logf("%d events, %d messages on the topic, %d duplicates, %d simulated crashes", len(ids), len(got), duplicates, simulatedCrashes.Load())
	assert.Positive(t, simulatedCrashes.Load(), "the test must actually crash batches")
	// Duplicates are the price of at-least-once (published before the crash, published again after); they are
	// logged above but not required: a crash can also hit a batch whose publish had not succeeded yet.
}

type failingPublisher struct{}

func (failingPublisher) Publish(_ context.Context, events []outbox.Event) []error {
	res := make([]error, len(events))
	for i := range res {
		res[i] = errors.New("kafka is down")
	}
	return res
}

// Failed deliveries are counted, delayed, and the event is eventually FAILED instead of being retried forever.
func TestFailingEventsEndUpFailedAfterMaxAttempts(t *testing.T) {
	db := openDB(t)
	prefix, ids := seed(t, db, 3, 1)
	p, m := poolWith(db, prefix, failingPublisher{}, 2, nil, 5, 0) // few attempts, no delay: we want it to give up fast

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	require.Eventually(t, func() bool { return countByStatus(t, db, prefix, "FAILED") == len(ids) }, 30*time.Second, 100*time.Millisecond)
	cancel()
	<-done

	var attempts int
	var lastErr string
	require.NoError(t, db.QueryRow("SELECT attempts, last_error FROM outbox_events WHERE id = :1", ids[0]).Scan(&attempts, &lastErr))
	assert.Equal(t, 5, attempts)
	assert.Contains(t, lastErr, "kafka is down")
	assert.EqualValues(t, 3, m.gaveUp.Load())
	assert.Zero(t, countByStatus(t, db, prefix, "SENT"))
}

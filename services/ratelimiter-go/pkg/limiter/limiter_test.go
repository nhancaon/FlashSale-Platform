package limiter_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/nhancaon/flashsale/services/ratelimiter-go/pkg/limiter"
)

var rdb *redis.Client

// TestMain uses TEST_REDIS_ADDR when set (CI, docker-based -race runs) and
// otherwise starts a throwaway Redis with Testcontainers.
func TestMain(m *testing.M) {
	ctx := context.Background()
	addr := os.Getenv("TEST_REDIS_ADDR")
	var cleanup func()
	if addr == "" {
		c, err := tcredis.Run(ctx, "redis:7-alpine")
		if err != nil {
			log.Fatalf("start redis container: %v", err)
		}
		cleanup = func() { _ = testcontainers.TerminateContainer(c) }
		ep, err := c.Endpoint(ctx, "")
		if err != nil {
			log.Fatalf("redis endpoint: %v", err)
		}
		addr = ep
	}
	rdb = redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("ping redis at %s: %v", addr, err)
	}
	code := m.Run()
	_ = rdb.Close()
	if cleanup != nil {
		cleanup()
	}
	os.Exit(code)
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	// Aligned to a minute so fixed-window boundaries are predictable.
	return &fakeClock{t: time.Unix(1_700_000_040, 0)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

var testSeq atomic.Uint64

// uniqueKey keeps tests isolated when they share one Redis.
func uniqueKey(t *testing.T) string {
	return fmt.Sprintf("%s-%d-%d", t.Name(), time.Now().UnixNano(), testSeq.Add(1))
}

var algorithms = []string{limiter.TokenBucket, limiter.SlidingWindow, limiter.FixedWindow}

func newLimiter(t *testing.T, algo string, clock limiter.Clock) limiter.Limiter {
	t.Helper()
	l, err := limiter.New(algo, rdb, clock)
	require.NoError(t, err)
	return l
}

func TestFactoryRejectsUnknownAlgorithm(t *testing.T) {
	_, err := limiter.New("leaky_bucket", rdb, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "leaky_bucket")
}

func TestRejectsInvalidParameters(t *testing.T) {
	l := newLimiter(t, limiter.FixedWindow, nil)
	_, err := l.Allow(context.Background(), "k", 0, time.Second)
	assert.Error(t, err)
	_, err = l.Allow(context.Background(), "k", 5, 0)
	assert.Error(t, err)
}

// Behaviour every algorithm must share.
func TestCommonBehaviour(t *testing.T) {
	for _, algo := range algorithms {
		t.Run(algo, func(t *testing.T) {
			ctx := context.Background()
			clock := newFakeClock()
			l := newLimiter(t, algo, clock)
			key := uniqueKey(t)
			const limit = 5
			window := 10 * time.Second

			for i := 1; i <= limit; i++ {
				r, err := l.Allow(ctx, key, limit, window)
				require.NoError(t, err)
				assert.True(t, r.Allowed, "request %d should pass", i)
				assert.EqualValues(t, limit-i, r.Remaining)
				assert.Zero(t, r.RetryAfterMs)
			}

			r, err := l.Allow(ctx, key, limit, window)
			require.NoError(t, err)
			assert.False(t, r.Allowed)
			assert.Zero(t, r.Remaining)
			assert.Positive(t, r.RetryAfterMs)
			assert.LessOrEqual(t, r.RetryAfterMs, window.Milliseconds())

			// Waiting the advertised retry-after must let a request through.
			clock.Advance(time.Duration(r.RetryAfterMs) * time.Millisecond)
			r, err = l.Allow(ctx, key, limit, window)
			require.NoError(t, err)
			assert.True(t, r.Allowed, "request after retryAfter should pass")

			// Other keys are unaffected.
			other, err := l.Allow(ctx, uniqueKey(t), limit, window)
			require.NoError(t, err)
			assert.True(t, other.Allowed)
		})
	}
}

// 1000 goroutines race for one key: exactly `limit` may pass, never more.
func TestConcurrencyNeverExceedsLimit(t *testing.T) {
	for _, algo := range algorithms {
		t.Run(algo, func(t *testing.T) {
			const (
				workers = 1000
				limit   = 100
			)
			l := newLimiter(t, algo, newFakeClock()) // frozen clock: no refill during the test
			key := uniqueKey(t)
			var allowed, rejected atomic.Int64
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					r, err := l.Allow(context.Background(), key, limit, time.Minute)
					if err != nil {
						t.Errorf("allow: %v", err)
						return
					}
					if r.Allowed {
						allowed.Add(1)
					} else {
						rejected.Add(1)
					}
				}()
			}
			close(start)
			wg.Wait()
			assert.EqualValues(t, limit, allowed.Load())
			assert.EqualValues(t, workers-limit, rejected.Load())
		})
	}
}

func TestFixedWindowAllowsBurstAcrossBoundary(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	l := newLimiter(t, limiter.FixedWindow, clock)
	key := uniqueKey(t)
	const limit = 3
	window := 10 * time.Second

	clock.Advance(9 * time.Second) // 1s before the window ends
	for i := 0; i < limit; i++ {
		r, err := l.Allow(ctx, key, limit, window)
		require.NoError(t, err)
		require.True(t, r.Allowed)
	}
	clock.Advance(1100 * time.Millisecond) // just into the next window
	for i := 0; i < limit; i++ {
		r, err := l.Allow(ctx, key, limit, window)
		require.NoError(t, err)
		assert.True(t, r.Allowed, "known weakness: 2x limit within ~1s across a boundary")
	}
}

func TestSlidingWindowHasNoBoundaryBurst(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	l := newLimiter(t, limiter.SlidingWindow, clock)
	key := uniqueKey(t)
	const limit = 3
	window := 10 * time.Second

	clock.Advance(9 * time.Second)
	for i := 0; i < limit; i++ {
		r, err := l.Allow(ctx, key, limit, window)
		require.NoError(t, err)
		require.True(t, r.Allowed)
	}
	clock.Advance(1100 * time.Millisecond)
	r, err := l.Allow(ctx, key, limit, window)
	require.NoError(t, err)
	assert.False(t, r.Allowed, "the earlier requests are still inside the sliding window")
	assert.EqualValues(t, 8900, r.RetryAfterMs)
}

func TestTokenBucketRefillsGradually(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	l := newLimiter(t, limiter.TokenBucket, clock)
	key := uniqueKey(t)
	const limit = 10
	window := 10 * time.Second // refill: 1 token per second

	for i := 0; i < limit; i++ {
		r, err := l.Allow(ctx, key, limit, window)
		require.NoError(t, err)
		require.True(t, r.Allowed)
	}
	r, err := l.Allow(ctx, key, limit, window)
	require.NoError(t, err)
	require.False(t, r.Allowed)
	assert.EqualValues(t, 1000, r.RetryAfterMs)

	clock.Advance(3 * time.Second) // 3 tokens back
	for i := 0; i < 3; i++ {
		r, err = l.Allow(ctx, key, limit, window)
		require.NoError(t, err)
		assert.True(t, r.Allowed, "refilled token %d", i+1)
	}
	r, err = l.Allow(ctx, key, limit, window)
	require.NoError(t, err)
	assert.False(t, r.Allowed)

	clock.Advance(time.Hour) // never exceeds capacity
	r, err = l.Allow(ctx, key, limit, window)
	require.NoError(t, err)
	assert.True(t, r.Allowed)
	assert.EqualValues(t, limit-1, r.Remaining)
}

func TestKeyExpiresAfterWindow(t *testing.T) {
	ctx := context.Background()
	for _, algo := range algorithms {
		t.Run(algo, func(t *testing.T) {
			l := newLimiter(t, algo, nil) // real clock
			key := uniqueKey(t)
			_, err := l.Allow(ctx, key, 5, 300*time.Millisecond)
			require.NoError(t, err)
			require.Eventually(t, func() bool {
				keys, err := rdb.Keys(ctx, "rl:"+algo+":"+key+"*").Result()
				return err == nil && len(keys) == 0
			}, 3*time.Second, 50*time.Millisecond, "state must not leak in Redis")
		})
	}
}

// Package limiter implements the rate limit algorithms as interchangeable
// strategies. Each strategy keeps its state in Redis and decides atomically in
// a Lua script, so many service instances can share one limit.
package limiter

import (
	"context"
	"embed"
	"fmt"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"
)

//go:embed scripts/*.lua
var scriptFS embed.FS

// Algorithm names accepted by New and the RATELIMIT_ALGORITHM setting.
const (
	TokenBucket   = "token_bucket"
	SlidingWindow = "sliding_window"
	FixedWindow   = "fixed_window"
)

// Result is the decision for one request.
type Result struct {
	Allowed      bool
	Remaining    int64
	RetryAfterMs int64
}

// Limiter decides whether a request for key may proceed.
type Limiter interface {
	Name() string
	Allow(ctx context.Context, key string, limit int64, window time.Duration) (Result, error)
}

// Clock lets tests control time. Time is passed to the Lua script as an
// argument (instead of using Redis TIME) so tests can be deterministic.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// SystemClock returns a Clock backed by the wall clock.
func SystemClock() Clock { return systemClock{} }

// New is the factory that builds a Limiter from its configured name.
func New(algorithm string, rdb redis.Scripter, clock Clock) (Limiter, error) {
	if clock == nil {
		clock = SystemClock()
	}
	switch algorithm {
	case TokenBucket:
		return &tokenBucket{base: newBase(TokenBucket, "token_bucket.lua", rdb, clock)}, nil
	case SlidingWindow:
		return &slidingWindow{base: newBase(SlidingWindow, "sliding_window.lua", rdb, clock), memberSeq: new(atomic.Uint64), instance: instanceID()}, nil
	case FixedWindow:
		return &fixedWindow{base: newBase(FixedWindow, "fixed_window.lua", rdb, clock)}, nil
	default:
		return nil, fmt.Errorf("unknown rate limit algorithm %q (want %s, %s or %s)",
			algorithm, TokenBucket, SlidingWindow, FixedWindow)
	}
}

type base struct {
	name   string
	script *redis.Script
	rdb    redis.Scripter
	clock  Clock
}

func newBase(name, file string, rdb redis.Scripter, clock Clock) base {
	src, err := scriptFS.ReadFile("scripts/" + file)
	if err != nil {
		panic(fmt.Sprintf("embedded script %s missing: %v", file, err)) // build-time bug
	}
	return base{name: name, script: redis.NewScript(string(src)), rdb: rdb, clock: clock}
}

func (b base) Name() string { return b.name }

func (b base) redisKey(key string) string { return "rl:" + b.name + ":" + key }

func (b base) run(ctx context.Context, key string, extra []any, limit int64, window time.Duration) (Result, error) {
	if limit <= 0 || window < time.Millisecond {
		return Result{}, fmt.Errorf("limit must be > 0 and window >= 1ms (got %d, %s)", limit, window)
	}
	nowMs := b.clock.Now().UnixMilli()
	args := append([]any{nowMs, limit, window.Milliseconds()}, extra...)
	raw, err := b.script.Run(ctx, b.rdb, []string{b.redisKey(key)}, args...).Int64Slice()
	if err != nil {
		return Result{}, fmt.Errorf("%s script: %w", b.name, err)
	}
	if len(raw) != 3 {
		return Result{}, fmt.Errorf("%s script returned %d values, want 3", b.name, len(raw))
	}
	return Result{Allowed: raw[0] == 1, Remaining: raw[1], RetryAfterMs: raw[2]}, nil
}

type tokenBucket struct{ base }

func (t *tokenBucket) Allow(ctx context.Context, key string, limit int64, window time.Duration) (Result, error) {
	return t.run(ctx, key, nil, limit, window)
}

type fixedWindow struct{ base }

func (f *fixedWindow) Allow(ctx context.Context, key string, limit int64, window time.Duration) (Result, error) {
	return f.run(ctx, key, nil, limit, window)
}

type slidingWindow struct {
	base
	memberSeq *atomic.Uint64
	instance  string
}

func (s *slidingWindow) Allow(ctx context.Context, key string, limit int64, window time.Duration) (Result, error) {
	// Sorted-set members must be unique, otherwise two requests in the same
	// millisecond would collapse into one entry and under-count.
	member := s.instance + "-" + strconv.FormatUint(s.memberSeq.Add(1), 36)
	return s.run(ctx, key, []any{member}, limit, window)
}

// Package cache is the cache-aside layer for stock reads. Value format "available:reserved".
// Redis is best effort: any failure counts as a miss, so Oracle stays the source of truth. Entries are
// deleted after every committed change and also expire quickly (a concurrent read can re-fill an old
// value right after a delete, so staleness is bounded by the TTL).
package cache

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type Stock struct {
	SKU       string
	Available int64
	Reserved  int64
}

// Cache is what the service needs; Noop disables caching.
type Cache interface {
	Get(ctx context.Context, sku string) (Stock, bool)
	Put(ctx context.Context, s Stock)
	Invalidate(ctx context.Context, sku string)
}

type Noop struct{}

func (Noop) Get(context.Context, string) (Stock, bool) { return Stock{}, false }
func (Noop) Put(context.Context, Stock)                {}
func (Noop) Invalidate(context.Context, string)        {}

type Redis struct {
	rdb    *redis.Client
	ttl    time.Duration
	log    *slog.Logger
	OnHit  func()
	OnMiss func()
}

func NewRedis(rdb *redis.Client, ttl time.Duration, log *slog.Logger) *Redis {
	return &Redis{rdb: rdb, ttl: ttl, log: log, OnHit: func() {}, OnMiss: func() {}}
}

func key(sku string) string { return "inv:stock:" + sku }

func (r *Redis) Get(ctx context.Context, sku string) (Stock, bool) {
	v, err := r.rdb.Get(ctx, key(sku)).Result()
	if err == nil {
		avail, rsv, ok := strings.Cut(v, ":")
		a, errA := strconv.ParseInt(avail, 10, 64)
		b, errB := strconv.ParseInt(rsv, 10, 64)
		if ok && errA == nil && errB == nil {
			r.OnHit()
			return Stock{SKU: sku, Available: a, Reserved: b}, true
		}
	} else if !errors.Is(err, redis.Nil) {
		r.log.Warn("stock cache read failed, falling back to Oracle", "error", err)
	}
	r.OnMiss()
	return Stock{}, false
}

func (r *Redis) Put(ctx context.Context, s Stock) {
	if err := r.rdb.Set(ctx, key(s.SKU), fmt.Sprintf("%d:%d", s.Available, s.Reserved), r.ttl).Err(); err != nil {
		r.log.Warn("stock cache write failed", "error", err)
	}
}

func (r *Redis) Invalidate(ctx context.Context, sku string) {
	if err := r.rdb.Del(ctx, key(sku)).Err(); err != nil {
		r.log.Warn("stock cache invalidate failed (entry expires by TTL)", "ttl", r.ttl, "error", err)
	}
}

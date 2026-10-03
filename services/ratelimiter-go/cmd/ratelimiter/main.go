package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/nhancaon/flashsale/services/ratelimiter-go/internal/limiter"
	"github.com/nhancaon/flashsale/services/ratelimiter-go/internal/server"
)

type redisPinger struct{ c *redis.Client }

func (p redisPinger) Ping(ctx context.Context) error { return p.c.Ping(ctx).Err() }

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	algo := env("RATELIMIT_ALGORITHM", limiter.TokenBucket)
	rdb := redis.NewClient(&redis.Options{
		Addr:         env("REDIS_ADDR", "localhost:6380"),
		DialTimeout:  time.Second,
		ReadTimeout:  500 * time.Millisecond,
		WriteTimeout: 500 * time.Millisecond,
		PoolSize:     envInt("REDIS_POOL_SIZE", 50),
	})
	defer rdb.Close()

	lim, err := limiter.New(algo, rdb, limiter.SystemClock())
	if err != nil {
		logger.Error("invalid configuration", "error", err)
		os.Exit(2)
	}

	srv := server.New(server.Config{
		Limiter:      lim,
		Ready:        redisPinger{rdb},
		CheckTimeout: time.Duration(envInt("CHECK_TIMEOUT_MS", 200)) * time.Millisecond,
		Logger:       logger,
	})
	httpSrv := &http.Server{
		Addr:              ":" + env("PORT", "8081"),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		logger.Info("listening", "addr", httpSrv.Addr, "algorithm", algo)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

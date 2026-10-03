package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
	_ "github.com/sijms/go-ora/v2"

	"github.com/nhancaon/flashsale/services/inventory-go/internal/cache"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/server"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/service"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/store"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/strategy"
)

type dbPinger struct{ db *sql.DB }

func (p dbPinger) Ping(ctx context.Context) error { return p.db.PingContext(ctx) }

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)
	if err := run(logger); err != nil {
		logger.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	password := os.Getenv("DB_PASSWORD") // no default on purpose: secrets come from the environment
	if password == "" {
		return errors.New("DB_PASSWORD is required")
	}
	dsn := fmt.Sprintf("oracle://%s:%s@%s:%s/%s",
		url.QueryEscape(env("DB_USER", "flashsale")), url.QueryEscape(password),
		env("DB_HOST", "localhost"), env("DB_PORT", "1521"), env("DB_SERVICE", "FREEPDB1"))
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		return fmt.Errorf("open oracle: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(envInt("DB_POOL_MAX", 20))
	db.SetMaxIdleConns(envInt("DB_POOL_MIN_IDLE", 5))
	db.SetConnMaxLifetime(30 * time.Minute)

	strat, err := strategy.New(env("STOCK_STRATEGY", strategy.Atomic), envInt("OPTIMISTIC_MAX_RETRIES", 50))
	if err != nil {
		return err
	}

	reg := prometheus.NewRegistry()
	registerDBPoolMetrics(reg, db)

	var c cache.Cache = cache.Noop{}
	var rdb *redis.Client
	if env("CACHE_ENABLED", "true") == "true" {
		rdb = redis.NewClient(&redis.Options{
			Addr:         env("REDIS_ADDR", "localhost:6380"),
			DialTimeout:  500 * time.Millisecond,
			ReadTimeout:  100 * time.Millisecond,
			WriteTimeout: 100 * time.Millisecond,
		})
		defer rdb.Close()
		rc := cache.NewRedis(rdb, time.Duration(envInt("CACHE_TTL_MS", 2000))*time.Millisecond, logger)
		hits := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "inventory_cache_total", Help: "Stock cache lookups."}, []string{"result"})
		reg.MustRegister(hits)
		rc.OnHit = func() { hits.WithLabelValues("hit").Inc() }
		rc.OnMiss = func() { hits.WithLabelValues("miss").Inc() }
		c = rc
	}

	svc := service.New(store.NewOracle(db), strat, c)
	srv := server.New(server.Config{
		Service:        svc,
		Ready:          dbPinger{db},
		Logger:         logger,
		RequestTimeout: time.Duration(envInt("REQUEST_TIMEOUT_MS", 10000)) * time.Millisecond,
		Registry:       reg,
	})
	httpSrv := &http.Server{
		Addr:              ":" + env("PORT", "8083"),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		logger.Info("listening", "addr", httpSrv.Addr, "strategy", strat.Name(), "pool_max", envInt("DB_POOL_MAX", 20))
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

// registerDBPoolMetrics exposes database/sql pool statistics (the Go counterpart of Hikari metrics).
func registerDBPoolMetrics(reg *prometheus.Registry, db *sql.DB) {
	gauge := func(name, help string, f func(sql.DBStats) float64) {
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, func() float64 { return f(db.Stats()) }))
	}
	gauge("db_pool_open_connections", "Open connections (in use + idle).", func(s sql.DBStats) float64 { return float64(s.OpenConnections) })
	gauge("db_pool_in_use_connections", "Connections currently in use.", func(s sql.DBStats) float64 { return float64(s.InUse) })
	gauge("db_pool_wait_count_total", "Total number of waits for a connection.", func(s sql.DBStats) float64 { return float64(s.WaitCount) })
	gauge("db_pool_wait_seconds_total", "Total time blocked waiting for a connection.", func(s sql.DBStats) float64 { return s.WaitDuration.Seconds() })
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

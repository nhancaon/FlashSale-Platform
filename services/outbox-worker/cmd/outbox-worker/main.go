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
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	_ "github.com/sijms/go-ora/v2"

	"github.com/nhancaon/flashsale/services/outbox-worker/internal/outbox"
	"github.com/nhancaon/flashsale/services/outbox-worker/internal/publisher"
)

type promMetrics struct {
	published, failed, gaveUp prometheus.Counter
	batch                     prometheus.Histogram
}

func (m promMetrics) Published(n int)               { m.published.Add(float64(n)) }
func (m promMetrics) Failed(n int)                  { m.failed.Add(float64(n)) }
func (m promMetrics) GaveUp(n int)                  { m.gaveUp.Add(float64(n)) }
func (m promMetrics) BatchDuration(d time.Duration) { m.batch.Observe(d.Seconds()) }

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
	batchSize := envInt("OUTBOX_BATCH_SIZE", 20)
	// PREFETCH_ROWS = batch size: Oracle locks rows as they are fetched, so a claim locks about one batch.
	dsn := fmt.Sprintf("oracle://%s:%s@%s:%s/%s?PREFETCH_ROWS=%d",
		url.QueryEscape(env("DB_USER", "flashsale")), url.QueryEscape(password),
		env("DB_HOST", "localhost"), env("DB_PORT", "1521"), env("DB_SERVICE", "FREEPDB1"), batchSize)
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		return fmt.Errorf("open oracle: %w", err)
	}
	defer db.Close()
	workers := envInt("OUTBOX_WORKERS", 3)
	db.SetMaxOpenConns(workers + 2)

	brokers := strings.Split(env("KAFKA_BROKERS", "localhost:29092"), ",")
	topic := env("KAFKA_TOPIC", "flashsale.order-events")
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := retry(ctx, 30*time.Second, func() error {
		return publisher.EnsureTopic(ctx, brokers[0], topic, envInt("KAFKA_PARTITIONS", 3))
	}); err != nil {
		return err
	}
	kafkaPub := publisher.NewKafka(brokers, topic)
	defer kafkaPub.Close()

	reg := prometheus.NewRegistry()
	m := promMetrics{
		published: prometheus.NewCounter(prometheus.CounterOpts{Name: "outbox_published_total", Help: "Events delivered to Kafka."}),
		failed:    prometheus.NewCounter(prometheus.CounterOpts{Name: "outbox_failed_attempts_total", Help: "Failed delivery attempts."}),
		gaveUp:    prometheus.NewCounter(prometheus.CounterOpts{Name: "outbox_gave_up_total", Help: "Events moved to FAILED."}),
		batch: prometheus.NewHistogram(prometheus.HistogramOpts{Name: "outbox_batch_duration_seconds", Help: "Claim + publish + commit time.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5}}),
	}
	pending := prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "outbox_pending_events", Help: "NEW events waiting (relay lag)."},
		func() float64 { return pendingCount(db) })
	reg.MustRegister(m.published, m.failed, m.gaveUp, m.batch, pending)

	proc := &outbox.Processor{
		Source:      &outbox.Oracle{DB: db},
		Publisher:   kafkaPub,
		BatchSize:   batchSize,
		MaxAttempts: envInt("OUTBOX_MAX_ATTEMPTS", 20),
		Metrics:     m,
		Log:         logger,
		Backoff:     outbox.ExponentialBackoff(time.Minute),
	}
	pool := &outbox.Pool{
		Workers:      workers,
		Processor:    proc,
		PollInterval: time.Duration(envInt("OUTBOX_POLL_MS", 200)) * time.Millisecond,
		MaxBackoff:   30 * time.Second,
		Log:          logger,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}`)) })
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		c, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := db.PingContext(c); err != nil {
			http.Error(w, `{"code":"NOT_READY","message":"oracle unreachable"}`, http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"status":"ready"}`))
	})
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	httpSrv := &http.Server{Addr: ":" + env("PORT", "8086"), Handler: mux, ReadHeaderTimeout: 2 * time.Second}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			stop()
		}
	}()

	logger.Info("outbox worker started", "workers", workers, "batch_size", batchSize, "topic", topic, "brokers", brokers)
	pool.Run(ctx) // returns after SIGTERM once every worker finished its in-flight batch
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func pendingCount(db *sql.DB) float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var n float64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM outbox_events WHERE status = 'NEW'").Scan(&n); err != nil {
		return -1
	}
	return n
}

// retry runs fn until it succeeds, ctx ends or the budget is spent (the broker may still be starting).
func retry(ctx context.Context, budget time.Duration, fn func() error) error {
	deadline := time.Now().Add(budget)
	for {
		err := fn()
		if err == nil || ctx.Err() != nil || time.Now().After(deadline) {
			return err
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
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

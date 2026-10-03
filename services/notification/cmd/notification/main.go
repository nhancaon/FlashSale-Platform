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
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/segmentio/kafka-go"
	_ "github.com/sijms/go-ora/v2"

	"github.com/nhancaon/flashsale/services/notification/internal/notify"
)

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
	db.SetMaxOpenConns(5)

	group := env("KAFKA_GROUP", "notification")
	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        strings.Split(env("KAFKA_BROKERS", "localhost:29092"), ","),
		GroupID:        group,
		Topic:          env("KAFKA_TOPIC", "flashsale.order-events"),
		MinBytes:       1,
		MaxBytes:       1 << 20,
		MaxWait:        500 * time.Millisecond,
		StartOffset:    kafka.FirstOffset, // a new group reads from the beginning: nothing is missed
		CommitInterval: 0,                 // we commit explicitly, after handling
	})
	defer reader.Close()

	reg := prometheus.NewRegistry()
	processed := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "notification_processed_total", Help: "Messages handled by result."}, []string{"result"})
	reg.MustRegister(processed)
	for _, r := range []notify.Result{notify.Sent, notify.Skipped, notify.Duplicate, notify.Poison} {
		processed.WithLabelValues(string(r))
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
	httpSrv := &http.Server{Addr: ":" + env("PORT", "8087"), Handler: mux, ReadHeaderTimeout: 2 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			stop()
		}
	}()

	logger.Info("notification consumer started", "group", group)
	handler := &notify.Handler{Consumer: group, Store: &notify.Oracle{DB: db}}
	notify.Run(ctx, reader, handler, func(r notify.Result) { processed.WithLabelValues(string(r)).Inc() }, logger)

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

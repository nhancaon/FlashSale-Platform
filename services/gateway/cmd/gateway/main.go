package main

import (
	"context"
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

	"github.com/nhancaon/flashsale/services/gateway/internal/app"
	"github.com/nhancaon/flashsale/services/gateway/internal/auth"
	"github.com/nhancaon/flashsale/services/gateway/internal/health"
	"github.com/nhancaon/flashsale/services/gateway/internal/proxy"
	"github.com/nhancaon/flashsale/services/gateway/internal/ratelimit"
	"github.com/nhancaon/flashsale/services/ratelimiter-go/pkg/limiter"
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
	// Secrets come from the environment only, without defaults.
	issuer, err := auth.NewIssuer(os.Getenv("JWT_SECRET"), time.Duration(envInt("JWT_TTL_SECONDS", 900))*time.Second)
	if err != nil {
		return fmt.Errorf("JWT_SECRET: %w", err)
	}
	demoPassword := os.Getenv("DEMO_PASSWORD")
	if demoPassword == "" {
		return errors.New("DEMO_PASSWORD is required")
	}

	orderURL, err := url.Parse(env("ORDER_URL", "http://localhost:8085"))
	if err != nil {
		return fmt.Errorf("ORDER_URL: %w", err)
	}
	inventoryURL, err := url.Parse(env("INVENTORY_URL", "http://localhost:8083"))
	if err != nil {
		return fmt.Errorf("INVENTORY_URL: %w", err)
	}

	checks := map[string]health.Check{}
	httpClient := &http.Client{Timeout: time.Second}
	checks["order"] = health.HTTPCheck(httpClient, orderURL.String()+"/readyz")
	checks["inventory"] = health.HTTPCheck(httpClient, inventoryURL.String()+"/readyz")

	var lim limiter.Limiter
	switch mode := env("RATELIMIT_MODE", "embedded"); mode {
	case "embedded": // the ratelimiter-go library inside this process, Redis as shared state
		rdb := redis.NewClient(&redis.Options{Addr: env("REDIS_ADDR", "localhost:6380"),
			DialTimeout: 500 * time.Millisecond, ReadTimeout: 200 * time.Millisecond, WriteTimeout: 200 * time.Millisecond})
		defer rdb.Close()
		lim, err = limiter.New(env("RATELIMIT_ALGORITHM", limiter.TokenBucket), rdb, limiter.SystemClock())
		if err != nil {
			return err
		}
		checks["redis"] = func(ctx context.Context) error { return rdb.Ping(ctx).Err() }
	case "remote": // the standalone ratelimiter service (Go or Java)
		base := env("RATELIMITER_URL", "http://localhost:8081")
		lim = ratelimit.NewRemote(base, 300*time.Millisecond)
		checks["ratelimiter"] = health.HTTPCheck(httpClient, base+"/readyz")
	default:
		return fmt.Errorf("RATELIMIT_MODE must be embedded or remote, got %q", mode)
	}

	timeout := time.Duration(envInt("UPSTREAM_TIMEOUT_MS", 8000)) * time.Millisecond
	reg := prometheus.NewRegistry()
	handler := app.New(app.Config{
		Issuer:      issuer,
		Credentials: auth.NewCredentials(demoPassword),

		Limiter:        lim,
		FailOpen:       env("RATELIMIT_FAIL_OPEN", "true") == "true",
		LimiterTimeout: 250 * time.Millisecond,
		IPLimit:        int64(envInt("RL_IP_LIMIT", 100)),
		IPWindow:       time.Duration(envInt("RL_IP_WINDOW_SEC", 1)) * time.Second,
		UserLimit:      int64(envInt("RL_USER_LIMIT", 20)),
		UserWindow:     time.Duration(envInt("RL_USER_WINDOW_SEC", 1)) * time.Second,
		TrustProxy:     env("TRUST_PROXY", "false") == "true",

		Order: proxy.Upstream{Name: "order", Target: orderURL, StripPrefix: "/api/orders", AddPrefix: "/v1/orders",
			Methods: []string{http.MethodGet, http.MethodPost}, Timeout: timeout, BreakerOpen: 10 * time.Second, BreakerTrips: 10},
		Inventory: proxy.Upstream{Name: "inventory", Target: inventoryURL, StripPrefix: "/api/inventory", AddPrefix: "/v1/inventory",
			Methods: []string{http.MethodGet}, Timeout: 3 * time.Second, BreakerOpen: 10 * time.Second, BreakerTrips: 10},

		Checks:   checks,
		Logger:   logger,
		Registry: reg,
	})

	srv := &http.Server{
		Addr:              ":" + env("PORT", "8088"),
		Handler:           handler,
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      timeout + 5*time.Second,
		IdleTimeout:       60 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() {
		logger.Info("gateway listening", "addr", srv.Addr, "ratelimit_mode", env("RATELIMIT_MODE", "embedded"))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
	}
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
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

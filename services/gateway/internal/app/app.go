// Package app assembles the gateway: the chain of middleware around the routes.
//
// Request path for /api/*:
//
//	request id -> logging+metrics -> IP rate limit -> JWT -> user rate limit -> circuit breaker -> reverse proxy
//
// The IP limit runs before the JWT check so unauthenticated floods are cheap to reject; the user limit needs the
// identity the JWT step established.
package app

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/nhancaon/flashsale/services/gateway/internal/auth"
	"github.com/nhancaon/flashsale/services/gateway/internal/health"
	"github.com/nhancaon/flashsale/services/gateway/internal/middleware"
	"github.com/nhancaon/flashsale/services/gateway/internal/proxy"
	"github.com/nhancaon/flashsale/services/ratelimiter-go/pkg/limiter"
)

type Config struct {
	Issuer      *auth.Issuer
	Credentials auth.Credentials

	Limiter        limiter.Limiter
	FailOpen       bool
	LimiterTimeout time.Duration
	IPLimit        int64
	IPWindow       time.Duration
	UserLimit      int64
	UserWindow     time.Duration
	TrustProxy     bool

	Order     proxy.Upstream
	Inventory proxy.Upstream

	Checks map[string]health.Check
	Logger *slog.Logger
	// Registry receives all metrics; defaults to a fresh one.
	Registry *prometheus.Registry
}

// New builds the handler.
func New(cfg Config) http.Handler {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Registry == nil {
		cfg.Registry = prometheus.NewRegistry()
	}
	metrics := middleware.NewMetrics(cfg.Registry)
	rl := middleware.NewRateLimiter(cfg.Limiter, cfg.FailOpen, cfg.LimiterTimeout, cfg.Logger, cfg.Registry)
	breakerState := proxy.BreakerStateGauge(cfg.Registry)

	ipScope := middleware.Scope{Name: "ip", Limit: cfg.IPLimit, Window: cfg.IPWindow,
		Key: func(r *http.Request) string { return middleware.ClientIP(r, cfg.TrustProxy) }}
	userScope := middleware.Scope{Name: "user", Limit: cfg.UserLimit, Window: cfg.UserWindow, Key: middleware.UserKey}

	health := &health.Aggregator{Checks: cfg.Checks, Timeout: time.Second}
	orders := proxy.Handler(cfg.Order, cfg.Logger, breakerState)
	inventory := proxy.Handler(cfg.Inventory, cfg.Logger, breakerState)

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.Logging(cfg.Logger, metrics))

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Method(http.MethodGet, "/readyz", health.ReadyHandler())
	r.Handle("/metrics", promhttp.HandlerFor(cfg.Registry, promhttp.HandlerOpts{}))

	r.With(rl.Limit(ipScope)).Post("/auth/login", login(cfg))

	r.Route("/api", func(r chi.Router) {
		r.Use(rl.Limit(ipScope), middleware.Authenticate(cfg.Issuer.Verify), rl.Limit(userScope))
		r.Handle("/orders", orders)
		r.Handle("/orders/*", orders)
		r.Handle("/inventory/*", inventory)
	})
	return r
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// login is the simulated identity provider: any well formed username with the demo password gets a token.
func login(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			middleware.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "body must be JSON {\"username\", \"password\"}")
			return
		}
		if !cfg.Credentials.Check(req.Username, req.Password) {
			// One answer for "unknown user" and "wrong password", so usernames cannot be probed.
			middleware.WriteError(w, http.StatusUnauthorized, "INVALID_CREDENTIALS", "wrong username or password")
			return
		}
		token, exp, err := cfg.Issuer.Issue(req.Username)
		if err != nil {
			cfg.Logger.Error("could not sign token", "error", err)
			middleware.WriteError(w, http.StatusInternalServerError, "INTERNAL", "could not issue a token")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"accessToken": token, "tokenType": "Bearer", "expiresIn": int(time.Until(exp).Seconds()),
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

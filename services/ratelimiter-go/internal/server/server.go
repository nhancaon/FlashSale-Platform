// Package server exposes the rate limiter over HTTP (contract: POST /v1/check).
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/nhancaon/flashsale/services/ratelimiter-go/internal/limiter"
)

const (
	headerRequestID = "X-Request-Id"
	maxBodyBytes    = 1 << 20
	maxKeyLen       = 256
)

// Pinger lets /readyz check the backing store without importing Redis here.
type Pinger interface {
	Ping(ctx context.Context) error
}

type Config struct {
	Limiter      limiter.Limiter
	Ready        Pinger
	CheckTimeout time.Duration
	Logger       *slog.Logger
}

type Server struct {
	cfg      Config
	registry *prometheus.Registry
	allowed  prometheus.Counter
	rejected prometheus.Counter
	errors   prometheus.Counter
	latency  prometheus.Histogram
}

func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.CheckTimeout <= 0 {
		cfg.CheckTimeout = 200 * time.Millisecond
	}
	algo := prometheus.Labels{"algorithm": cfg.Limiter.Name()}
	reg := prometheus.NewRegistry()
	s := &Server{
		cfg:      cfg,
		registry: reg,
		allowed: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ratelimit_allowed_total", Help: "Requests allowed by the rate limiter.", ConstLabels: algo}),
		rejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ratelimit_rejected_total", Help: "Requests rejected by the rate limiter.", ConstLabels: algo}),
		errors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "ratelimit_errors_total", Help: "Checks that failed because the backing store errored.", ConstLabels: algo}),
		latency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "ratelimit_check_duration_seconds", Help: "Latency of limiter decisions.",
			ConstLabels: algo, Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1}}),
	}
	reg.MustRegister(s.allowed, s.rejected, s.errors, s.latency)
	return s
}

// Handler builds the HTTP handler with the middleware chain
// request id -> access log -> routes.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(requestID, s.accessLog)
	r.Post("/v1/check", s.check)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, map[string]string{"status": "ok"}) })
	r.Get("/readyz", s.readyz)
	r.Handle("/metrics", promhttp.HandlerFor(s.registry, promhttp.HandlerOpts{}))
	return r
}

type checkRequest struct {
	Key       string `json:"key"`
	Limit     int64  `json:"limit"`
	WindowSec int64  `json:"windowSec"`
}

type checkResponse struct {
	Allowed      bool  `json:"allowed"`
	Remaining    int64 `json:"remaining"`
	RetryAfterMs int64 `json:"retryAfterMs"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (s *Server) check(w http.ResponseWriter, r *http.Request) {
	var req checkRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "body must be valid JSON: "+err.Error())
		return
	}
	if msg := validate(req); msg != "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", msg)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.CheckTimeout)
	defer cancel()
	start := time.Now()
	res, err := s.cfg.Limiter.Allow(ctx, req.Key, req.Limit, time.Duration(req.WindowSec)*time.Second)
	s.latency.Observe(time.Since(start).Seconds())
	if err != nil {
		s.errors.Inc()
		code := http.StatusServiceUnavailable
		if errors.Is(err, context.Canceled) {
			code = 499 // client went away
		}
		s.cfg.Logger.Error("limiter failure", "request_id", r.Header.Get(headerRequestID), "error", err)
		writeError(w, code, "BACKEND_UNAVAILABLE", "rate limit store unavailable")
		return
	}
	if res.Allowed {
		s.allowed.Inc()
	} else {
		s.rejected.Inc()
	}
	writeJSON(w, http.StatusOK, checkResponse{Allowed: res.Allowed, Remaining: res.Remaining, RetryAfterMs: res.RetryAfterMs})
}

func validate(req checkRequest) string {
	switch {
	case req.Key == "":
		return "key is required"
	case len(req.Key) > maxKeyLen:
		return "key is too long"
	case req.Limit <= 0:
		return "limit must be greater than 0"
	case req.WindowSec <= 0:
		return "windowSec must be greater than 0"
	}
	return ""
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Ready != nil {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if err := s.cfg.Ready.Ping(ctx); err != nil {
			writeError(w, http.StatusServiceUnavailable, "NOT_READY", "redis unreachable")
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(headerRequestID)
		if id == "" {
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
			r.Header.Set(headerRequestID, id)
		}
		w.Header().Set(headerRequestID, id)
		next.ServeHTTP(w, r)
	})
}

func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		if r.URL.Path == "/healthz" || r.URL.Path == "/metrics" {
			return // keep probe noise out of the logs
		}
		s.cfg.Logger.Info("request",
			"request_id", r.Header.Get(headerRequestID),
			"method", r.Method, "path", r.URL.Path,
			"status", rec.status, "duration_ms", time.Since(start).Milliseconds())
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Code: code, Message: msg})
}

// Package middleware holds the links of the gateway's chain of responsibility. Each one wraps the next handler and
// may answer the request itself (401, 429, ...) instead of passing it on.
package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
)

const HeaderRequestID = "X-Request-Id"

// Info is created per request by RequestID and shared down the chain, so the outer logging link can report what
// inner links learned (the authenticated user).
type Info struct {
	RequestID string
	User      string
}

type ctxKey struct{}

func InfoFrom(ctx context.Context) *Info {
	if i, ok := ctx.Value(ctxKey{}).(*Info); ok {
		return i
	}
	return &Info{}
}

var safeID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// RequestID keeps a well formed incoming id (so a trace can span services) or generates one.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if !safeID.MatchString(id) { // never echo arbitrary input into logs and headers
			b := make([]byte, 8)
			_, _ = rand.Read(b)
			id = hex.EncodeToString(b)
		}
		w.Header().Set(HeaderRequestID, id)
		r.Header.Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, &Info{RequestID: id})))
	})
}

// Metrics are the HTTP RED metrics of the gateway.
type Metrics struct {
	Requests *prometheus.CounterVec   // method, route, status
	Duration *prometheus.HistogramVec // method, route
}

func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		Requests: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "http_requests_total", Help: "Requests by route and status."}, []string{"method", "route", "status"}),
		Duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "http_request_duration_seconds", Help: "Request latency.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}}, []string{"method", "route"}),
	}
	reg.MustRegister(m.Requests, m.Duration)
	return m
}

type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }

// Unwrap lets http.ResponseController reach the real writer (flushing, deadlines).
func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Logging writes one JSON line per request and records the metrics. Probes and metrics scrapes are not logged.
func Logging(log *slog.Logger, m *Metrics) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &recorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)
			route := chi.RouteContext(r.Context()).RoutePattern()
			if route == "" {
				route = "unmatched"
			}
			elapsed := time.Since(start)
			if m != nil {
				m.Requests.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
				m.Duration.WithLabelValues(r.Method, route).Observe(elapsed.Seconds())
			}
			if route == "/healthz" || route == "/readyz" || route == "/metrics" {
				return
			}
			info := InfoFrom(r.Context())
			log.Info("request", "request_id", info.RequestID, "user", info.User, "method", r.Method, "route", route,
				"status", rec.status, "duration_ms", elapsed.Milliseconds())
		})
	}
}

// Authenticate requires "Authorization: Bearer <jwt>" and records the subject for the rest of the chain.
func Authenticate(verify func(token string) (string, error)) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h := r.Header.Get("Authorization")
			token, ok := strings.CutPrefix(h, "Bearer ")
			if !ok || token == "" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="flashsale"`)
				WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing bearer token")
				return
			}
			user, err := verify(token)
			if err != nil {
				w.Header().Set("WWW-Authenticate", `Bearer realm="flashsale", error="invalid_token"`)
				WriteError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or expired token")
				return
			}
			InfoFrom(r.Context()).User = user
			next.ServeHTTP(w, r)
		})
	}
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// WriteError writes the shared error shape {"code": "...", "message": "..."}.
func WriteError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorBody{code, message})
}

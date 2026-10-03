package middleware

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/nhancaon/flashsale/services/ratelimiter-go/pkg/limiter"
)

// Scope describes one rate limit: what identifies the caller and how much is allowed.
type Scope struct {
	Name   string // "ip" or "user" (metric label)
	Limit  int64
	Window time.Duration
	// Key returns the identifier to limit, or "" to skip this scope for the request.
	Key func(r *http.Request) string
}

// RateLimiter enforces scopes through a limiter.Limiter (the embedded library or the remote ratelimiter service).
type RateLimiter struct {
	Limiter  limiter.Limiter
	FailOpen bool // limiter unavailable: true lets the request through, false answers 503
	Log      *slog.Logger
	Timeout  time.Duration

	Rejected *prometheus.CounterVec // scope
	Errors   prometheus.Counter
}

func NewRateLimiter(l limiter.Limiter, failOpen bool, timeout time.Duration, log *slog.Logger, reg prometheus.Registerer) *RateLimiter {
	rl := &RateLimiter{Limiter: l, FailOpen: failOpen, Log: log, Timeout: timeout,
		Rejected: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "gateway_ratelimit_rejected_total", Help: "Requests rejected with 429."}, []string{"scope"}),
		Errors:   prometheus.NewCounter(prometheus.CounterOpts{Name: "gateway_ratelimit_errors_total", Help: "Limiter calls that failed."}),
	}
	reg.MustRegister(rl.Rejected, rl.Errors)
	return rl
}

// Limit returns the middleware for the given scopes, checked in order; the first rejection answers 429.
func (rl *RateLimiter) Limit(scopes ...Scope) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, s := range scopes {
				key := s.Key(r)
				if key == "" {
					continue
				}
				ctx, cancel := contextWithTimeout(r, rl.Timeout)
				res, err := rl.Limiter.Allow(ctx, s.Name+":"+key, s.Limit, s.Window)
				cancel()
				if err != nil {
					rl.Errors.Inc()
					rl.Log.Error("rate limiter unavailable", "request_id", InfoFrom(r.Context()).RequestID, "scope", s.Name, "fail_open", rl.FailOpen, "error", err)
					if rl.FailOpen {
						continue
					}
					w.Header().Set("Retry-After", "1")
					WriteError(w, http.StatusServiceUnavailable, "RATE_LIMITER_UNAVAILABLE", "rate limiting is unavailable, try again shortly")
					return
				}
				w.Header().Set("X-RateLimit-Limit", strconv.FormatInt(s.Limit, 10))
				w.Header().Set("X-RateLimit-Remaining", strconv.FormatInt(res.Remaining, 10))
				if !res.Allowed {
					rl.Rejected.WithLabelValues(s.Name).Inc()
					// Retry-After is whole seconds, rounded up, at least 1.
					secs := int64(math.Max(1, math.Ceil(float64(res.RetryAfterMs)/1000)))
					w.Header().Set("Retry-After", strconv.FormatInt(secs, 10))
					WriteError(w, http.StatusTooManyRequests, "RATE_LIMITED", "too many requests ("+s.Name+" limit), retry after "+strconv.FormatInt(secs, 10)+"s")
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ClientIP returns the caller's address. X-Forwarded-For is honoured only behind a trusted proxy; otherwise any
// client could pick its own rate limit key by sending the header.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
				return ip.String()
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// UserKey is the Scope key function for authenticated users.
func UserKey(r *http.Request) string { return InfoFrom(r.Context()).User }

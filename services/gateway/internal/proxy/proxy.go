// Package proxy forwards authenticated requests to an upstream service through a circuit breaker.
package proxy

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/sony/gobreaker/v2"

	"github.com/nhancaon/flashsale/services/gateway/internal/middleware"
)

// Upstream is one backend service and how the gateway exposes it.
type Upstream struct {
	Name         string
	Target       *url.URL // base URL, for example http://order:8080
	StripPrefix  string   // public prefix removed from the path, for example /api/orders
	AddPrefix    string   // prefix added in front of the rest, for example /v1/orders
	Methods      []string // allowed methods; others get 405 (the gateway does not expose every upstream endpoint)
	Timeout      time.Duration
	BreakerOpen  time.Duration // how long the breaker stays open before probing
	BreakerTrips int           // minimum requests in the window before the failure ratio is judged
}

// errUpstream5xx marks an upstream answer that counts as a failure for the breaker but is still relayed to the client.
var errUpstream5xx = errors.New("upstream answered 5xx")

type breakerTransport struct {
	base http.RoundTripper
	cb   *gobreaker.CircuitBreaker[*http.Response]
}

func (t *breakerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.cb.Execute(func() (*http.Response, error) {
		resp, err := t.base.RoundTrip(req)
		if err != nil {
			return nil, err // connect error, timeout: a failure
		}
		if resp.StatusCode >= 500 {
			return resp, errUpstream5xx // a failure for the breaker, but the answer still goes to the client
		}
		return resp, nil // 2xx, 3xx and 4xx (including business 409s) are healthy answers
	})
	if errors.Is(err, errUpstream5xx) {
		return resp, nil
	}
	return resp, err
}

// BreakerStateGauge exposes each breaker as gateway_upstream_breaker_state{upstream} (0 closed, 1 half-open, 2 open).
func BreakerStateGauge(reg prometheus.Registerer) *prometheus.GaugeVec {
	g := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "gateway_upstream_breaker_state", Help: "0 closed, 1 half-open, 2 open."}, []string{"upstream"})
	reg.MustRegister(g)
	return g
}

// Handler builds the reverse proxy for the upstream.
func Handler(u Upstream, log *slog.Logger, state *prometheus.GaugeVec) http.Handler {
	transport := &http.Transport{
		Proxy:                 nil,
		DialContext:           (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   128,
		IdleConnTimeout:       60 * time.Second,
		ResponseHeaderTimeout: u.Timeout,
	}
	trips := max(u.BreakerTrips, 5)
	cb := gobreaker.NewCircuitBreaker[*http.Response](gobreaker.Settings{
		Name:        u.Name,
		MaxRequests: 5, // probes allowed while half-open
		Interval:    30 * time.Second,
		Timeout:     u.BreakerOpen,
		ReadyToTrip: func(c gobreaker.Counts) bool {
			return c.Requests >= uint32(trips) && float64(c.TotalFailures)/float64(c.Requests) >= 0.5
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			log.Warn("circuit breaker state change", "upstream", name, "from", from.String(), "to", to.String())
			if state != nil {
				state.WithLabelValues(name).Set(float64(stateValue(to)))
			}
		},
	})
	if state != nil {
		state.WithLabelValues(u.Name).Set(0)
	}
	allowed := map[string]bool{}
	for _, m := range u.Methods {
		allowed[m] = true
	}

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			rest := strings.TrimPrefix(pr.In.URL.Path, u.StripPrefix)
			pr.Out.URL.Scheme = u.Target.Scheme
			pr.Out.URL.Host = u.Target.Host
			pr.Out.URL.Path = u.AddPrefix + rest
			pr.Out.URL.RawPath = ""
			pr.Out.Host = u.Target.Host
			pr.SetXForwarded()
			// The user comes from the verified token only: whatever the client sent is discarded.
			pr.Out.Header.Del("X-User-Id")
			pr.Out.Header.Del("Authorization") // upstream services trust the gateway, not the raw token
			info := middleware.InfoFrom(pr.In.Context())
			pr.Out.Header.Set("X-User-Id", info.User)
			pr.Out.Header.Set(middleware.HeaderRequestID, info.RequestID)
		},
		Transport: &breakerTransport{base: transport, cb: cb},
		// ReverseProxy adds upstream headers on top of what the gateway already wrote. Upstreams echo the request id
		// too, so drop theirs: the response keeps exactly one X-Request-Id (set by the gateway).
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del(middleware.HeaderRequestID)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			info := middleware.InfoFrom(r.Context())
			switch {
			case errors.Is(err, gobreaker.ErrOpenState), errors.Is(err, gobreaker.ErrTooManyRequests):
				w.Header().Set("Retry-After", strconv.Itoa(int(u.BreakerOpen/time.Second)+1))
				middleware.WriteError(w, http.StatusServiceUnavailable, "UPSTREAM_UNAVAILABLE", u.Name+" is temporarily unavailable")
			case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
				log.Warn("upstream timeout", "request_id", info.RequestID, "upstream", u.Name, "error", err)
				middleware.WriteError(w, http.StatusGatewayTimeout, "UPSTREAM_TIMEOUT", u.Name+" did not answer in time")
			case errors.Is(err, context.Canceled):
				// the client went away: nothing useful to write
			default:
				log.Error("upstream error", "request_id", info.RequestID, "upstream", u.Name, "error", err)
				middleware.WriteError(w, http.StatusBadGateway, "BAD_GATEWAY", "could not reach "+u.Name)
			}
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Method] {
			w.Header().Set("Allow", strings.Join(u.Methods, ", "))
			middleware.WriteError(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", r.Method+" is not available on this route")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), u.Timeout)
		defer cancel()
		rp.ServeHTTP(w, r.WithContext(ctx))
	})
}

func stateValue(s gobreaker.State) int {
	switch s {
	case gobreaker.StateHalfOpen:
		return 1
	case gobreaker.StateOpen:
		return 2
	default:
		return 0
	}
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

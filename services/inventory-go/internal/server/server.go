// Package server exposes the inventory use cases over HTTP (the contract shared with inventory-java).
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/nhancaon/flashsale/services/inventory-go/internal/apperr"
	"github.com/nhancaon/flashsale/services/inventory-go/internal/service"
)

const (
	headerRequestID = "X-Request-Id"
	maxBodyBytes    = 1 << 20
	maxIDLen        = 64
	maxQty          = 1000
)

type Pinger interface {
	Ping(ctx context.Context) error
}

type Config struct {
	Service        *service.Service
	Ready          Pinger
	Logger         *slog.Logger
	RequestTimeout time.Duration
	Registry       *prometheus.Registry
}

type Server struct {
	cfg      Config
	reqTotal *prometheus.CounterVec
	reqDur   *prometheus.HistogramVec
	reserve  *prometheus.CounterVec
}

func New(cfg Config) *Server {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 10 * time.Second
	}
	if cfg.Registry == nil {
		cfg.Registry = prometheus.NewRegistry()
	}
	s := &Server{
		cfg: cfg,
		reqTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "http_requests_total", Help: "HTTP requests by route and status."}, []string{"method", "route", "status"}),
		reqDur: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "http_request_duration_seconds", Help: "HTTP request latency.",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}}, []string{"method", "route"}),
		// Same metric name and label as the Java service: inventory_reserve_total{result}.
		reserve: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inventory_reserve_total", Help: "Reserve attempts by result."}, []string{"result"}),
	}
	cfg.Registry.MustRegister(s.reqTotal, s.reqDur, s.reserve)
	for _, r := range []string{"ok", "out_of_stock", "error"} {
		s.reserve.WithLabelValues(r) // expose zero values so dashboards and tests see the series
	}
	return s
}

func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(requestID, s.observe)
	r.Get("/v1/inventory/{sku}", s.getStock)
	r.Post("/v1/inventory/reserve", s.reserveStock)
	r.Post("/v1/inventory/release", s.release)
	r.Post("/v1/inventory/confirm", s.confirm)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", s.readyz)
	r.Handle("/metrics", promhttp.HandlerFor(s.cfg.Registry, promhttp.HandlerOpts{}))
	return r
}

type reserveRequest struct {
	OrderID string `json:"orderId"`
	SKU     string `json:"sku"`
	Qty     *int64 `json:"qty"`
}

type orderSkuRequest struct {
	OrderID string `json:"orderId"`
	SKU     string `json:"sku"`
}

func (s *Server) getStock(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	v, err := s.cfg.Service.GetStock(ctx, chi.URLParam(r, "sku"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sku": v.SKU, "available": v.Available, "reserved": v.Reserved})
}

func (s *Server) reserveStock(w http.ResponseWriter, r *http.Request) {
	var req reserveRequest
	if err := decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := validateIDs(req.OrderID, req.SKU); err != nil {
		s.fail(w, r, err)
		return
	}
	if req.Qty == nil || *req.Qty < 1 || *req.Qty > maxQty {
		s.fail(w, r, apperr.InvalidRequest("qty must be between 1 and "+strconv.Itoa(maxQty)))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	id, err := s.cfg.Service.Reserve(ctx, req.OrderID, req.SKU, *req.Qty)
	switch {
	case err == nil:
		s.reserve.WithLabelValues("ok").Inc()
	case isCode(err, "OUT_OF_STOCK"):
		s.reserve.WithLabelValues("out_of_stock").Inc()
	default:
		s.reserve.WithLabelValues("error").Inc()
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"reservationId": id})
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	s.orderSku(w, r, s.cfg.Service.Release)
}
func (s *Server) confirm(w http.ResponseWriter, r *http.Request) {
	s.orderSku(w, r, s.cfg.Service.Confirm)
}

func (s *Server) orderSku(w http.ResponseWriter, r *http.Request, op func(context.Context, string, string) (string, error)) {
	var req orderSkuRequest
	if err := decode(w, r, &req); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := validateIDs(req.OrderID, req.SKU); err != nil {
		s.fail(w, r, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.RequestTimeout)
	defer cancel()
	status, err := op(ctx, req.OrderID, req.SKU)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if s.cfg.Ready != nil {
		if err := s.cfg.Ready.Ping(ctx); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, errBody("NOT_READY", "oracle unreachable"))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func isCode(err error, code string) bool {
	e, ok := apperr.As(err)
	return ok && e.Code == code
}

func validateIDs(orderID, sku string) *apperr.Error {
	for _, f := range []struct{ name, v string }{{"orderId", orderID}, {"sku", sku}} {
		if f.v == "" {
			return apperr.InvalidRequest(f.name + " is required")
		}
		if len(f.v) > maxIDLen {
			return apperr.InvalidRequest(f.name + " is too long")
		}
	}
	return nil
}

func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return apperr.InvalidRequest("body must be valid JSON with known fields")
	}
	return nil
}

// fail writes a business error as-is and hides anything else behind a generic 500.
func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if e, ok := apperr.As(err); ok {
		if e.Status >= 500 {
			s.cfg.Logger.Error("request failed", "request_id", r.Header.Get(headerRequestID), "code", e.Code, "error", err)
		}
		writeJSON(w, e.Status, errBody(e.Code, e.Message))
		return
	}
	var ctxErr = errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	e := apperr.Internal()
	if ctxErr {
		e = apperr.DBUnavailable()
	}
	s.cfg.Logger.Error("request failed", "request_id", r.Header.Get(headerRequestID), "code", e.Code, "error", err)
	writeJSON(w, e.Status, errBody(e.Code, e.Message))
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) { r.status = code; r.ResponseWriter.WriteHeader(code) }

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

// observe records RED metrics per route pattern and writes one JSON access log line.
func (s *Server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		elapsed := time.Since(start)
		s.reqTotal.WithLabelValues(r.Method, route, strconv.Itoa(rec.status)).Inc()
		s.reqDur.WithLabelValues(r.Method, route).Observe(elapsed.Seconds())
		if route == "/healthz" || route == "/metrics" {
			return
		}
		s.cfg.Logger.Info("request", "request_id", r.Header.Get(headerRequestID), "method", r.Method,
			"route", route, "status", rec.status, "duration_ms", elapsed.Milliseconds())
	})
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func errBody(code, msg string) errorBody { return errorBody{code, msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

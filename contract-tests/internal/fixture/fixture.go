// Package fixture gives the contract tests and the benchmark tool a black-box HTTP client for the
// inventory API plus direct Oracle access to create test data and check the truth in the database.
package fixture

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	_ "github.com/sijms/go-ora/v2"
)

// Env is the test configuration, read from the environment.
type Env struct {
	BaseURL  string // BASE_URL of the inventory service under test
	Strategy string // STRATEGY the service runs with (atomic | pessimistic | optimistic)
	DB       *sql.DB
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// Open reads BASE_URL, STRATEGY and DB_* settings and connects to Oracle.
func Open() (*Env, error) {
	base := os.Getenv("BASE_URL")
	if base == "" {
		return nil, fmt.Errorf("BASE_URL is required (e.g. http://localhost:8084)")
	}
	pw := os.Getenv("DB_PASSWORD")
	if pw == "" {
		return nil, fmt.Errorf("DB_PASSWORD is required")
	}
	dsn := fmt.Sprintf("oracle://%s:%s@%s:%s/%s",
		url.QueryEscape(getenv("DB_USER", "flashsale")), url.QueryEscape(pw),
		getenv("DB_HOST", "localhost"), getenv("DB_PORT", "1521"), getenv("DB_SERVICE", "FREEPDB1"))
	db, err := sql.Open("oracle", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("oracle ping: %w", err)
	}
	return &Env{BaseURL: base, Strategy: getenv("STRATEGY", "atomic"), DB: db}, nil
}

var seq atomic.Uint64

// NewSKU creates a product with the given stock and returns its sku and product id.
func (e *Env) NewSKU(ctx context.Context, stock int) (string, int64, error) {
	sku := fmt.Sprintf("T-%d-%d", time.Now().UnixNano(), seq.Add(1))
	tx, err := e.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT INTO product (sku, name, price) VALUES (:1, :2, :3)", sku, "contract test "+sku, 100); err != nil {
		return "", 0, err
	}
	var id int64
	if err := tx.QueryRowContext(ctx, "SELECT id FROM product WHERE sku = :1", sku).Scan(&id); err != nil {
		return "", 0, err
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO stock (product_id, available) VALUES (:1, :2)", id, stock); err != nil {
		return "", 0, err
	}
	return sku, id, tx.Commit()
}

// DBStock is the authoritative stock row.
type DBStock struct{ Available, Reserved int64 }

func (e *Env) DBStock(ctx context.Context, productID int64) (DBStock, error) {
	var s DBStock
	err := e.DB.QueryRowContext(ctx, "SELECT available, reserved FROM stock WHERE product_id = :1", productID).Scan(&s.Available, &s.Reserved)
	return s, err
}

// DBReservations returns count and total qty of reservations in a status.
func (e *Env) DBReservations(ctx context.Context, productID int64, status string) (count, qty int64, err error) {
	err = e.DB.QueryRowContext(ctx,
		"SELECT COUNT(*), NVL(SUM(qty), 0) FROM reservation WHERE product_id = :1 AND status = :2", productID, status).Scan(&count, &qty)
	return
}

// Client talks to the service over HTTP.
type Client struct{ base string }

var httpClient = &http.Client{
	Timeout: 60 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns: 1024, MaxIdleConnsPerHost: 1024, MaxConnsPerHost: maxConns(),
		DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
	},
}

func (e *Env) Client() *Client { return &Client{base: e.BaseURL} }

// Resp is a decoded response.
type Resp struct {
	Status int
	Body   map[string]any
	Raw    string
}

func (r Resp) Str(k string) string { s, _ := r.Body[k].(string); return s }
func (r Resp) Num(k string) int64  { f, _ := r.Body[k].(float64); return int64(f) }

func (c *Client) do(method, path string, payload any) (Resp, error) {
	var rd io.Reader
	if payload != nil {
		switch p := payload.(type) {
		case string:
			rd = bytes.NewBufferString(p)
		default:
			b, _ := json.Marshal(p)
			rd = bytes.NewReader(b)
		}
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return Resp{}, err
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := httpClient.Do(req)
	if err != nil {
		return Resp{}, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	out := Resp{Status: res.StatusCode, Raw: string(raw)}
	_ = json.Unmarshal(raw, &out.Body)
	return out, nil
}

func (c *Client) Get(sku string) (Resp, error) { return c.do("GET", "/v1/inventory/"+sku, nil) }
func (c *Client) Reserve(orderID, sku string, qty int) (Resp, error) {
	return c.do("POST", "/v1/inventory/reserve", map[string]any{"orderId": orderID, "sku": sku, "qty": qty})
}
func (c *Client) Release(orderID, sku string) (Resp, error) {
	return c.do("POST", "/v1/inventory/release", map[string]any{"orderId": orderID, "sku": sku})
}
func (c *Client) Confirm(orderID, sku string) (Resp, error) {
	return c.do("POST", "/v1/inventory/confirm", map[string]any{"orderId": orderID, "sku": sku})
}
func (c *Client) Raw(method, path, body string) (Resp, error) { return c.do(method, path, body) }

// maxConns caps simultaneous TCP connections (env MAX_CONNS, default 64). Thousands of requests
// stay in flight, but share a pool of connections, like traffic behind a gateway. Without the cap,
// a burst of 2000 new connections was refused by inventory-java on a Windows host (listen backlog).
func maxConns() int {
	if v, err := strconv.Atoi(os.Getenv("MAX_CONNS")); err == nil && v > 0 {
		return v
	}
	return 64
}

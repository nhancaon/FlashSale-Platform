package ratelimit_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nhancaon/flashsale/services/gateway/internal/ratelimit"
)

func TestRemoteSendsTheContractAndMapsTheAnswer(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/check", r.URL.Path)
		assert.Equal(t, http.MethodPost, r.Method)
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		_, _ = io.WriteString(w, `{"allowed":false,"remaining":0,"retryAfterMs":1500}`)
	}))
	defer srv.Close()

	res, err := ratelimit.NewRemote(srv.URL+"/", time.Second).Allow(context.Background(), "user:alice", 20, 2*time.Second)
	require.NoError(t, err)
	assert.False(t, res.Allowed)
	assert.EqualValues(t, 1500, res.RetryAfterMs)
	assert.Equal(t, "user:alice", got["key"])
	assert.EqualValues(t, 20, got["limit"])
	assert.EqualValues(t, 2, got["windowSec"], "the window is sent in seconds")
}

func TestRemoteErrors(t *testing.T) {
	status := 200
	body := `{"allowed":true}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	r := ratelimit.NewRemote(srv.URL, time.Second)

	status, body = 503, `{"code":"BACKEND_UNAVAILABLE"}`
	_, err := r.Allow(context.Background(), "k", 1, time.Second)
	assert.ErrorContains(t, err, "503")

	status, body = 200, `not json`
	_, err = r.Allow(context.Background(), "k", 1, time.Second)
	assert.Error(t, err)

	srv.Close()
	_, err = r.Allow(context.Background(), "k", 1, time.Second)
	assert.Error(t, err, "an unreachable limiter is an error, so the middleware can fail open or closed")
}

func TestRemoteHonoursTheTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(500 * time.Millisecond) }))
	defer srv.Close()
	start := time.Now()
	_, err := ratelimit.NewRemote(srv.URL, 100*time.Millisecond).Allow(context.Background(), "k", 1, time.Second)
	assert.Error(t, err)
	assert.Less(t, time.Since(start), 400*time.Millisecond)
}

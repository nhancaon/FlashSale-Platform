package middleware

import (
	"context"
	"net/http"
	"time"
)

func contextWithTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return context.WithCancel(r.Context())
	}
	return context.WithTimeout(r.Context(), d)
}

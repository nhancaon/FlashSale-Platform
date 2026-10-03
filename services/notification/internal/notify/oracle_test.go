package notify_test

// Integration test against the real Oracle of `make up` (+ `make db-migrate`). Needs DB_PASSWORD; skips otherwise.

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/sijms/go-ora/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nhancaon/flashsale/services/notification/internal/notify"
)

func openDB(t *testing.T) *sql.DB {
	t.Helper()
	pw := os.Getenv("DB_PASSWORD")
	if pw == "" {
		t.Skip("DB_PASSWORD not set: run `make up && make db-migrate` and use make test-go")
	}
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	dsn := fmt.Sprintf("oracle://%s:%s@%s:%s/%s", url.QueryEscape(get("DB_USER", "flashsale")), url.QueryEscape(pw),
		get("DB_HOST", "localhost"), get("DB_PORT", "1521"), get("DB_SERVICE", "FREEPDB1"))
	db, err := sql.Open("oracle", dsn)
	require.NoError(t, err)
	db.SetMaxOpenConns(10)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func notificationCount(t *testing.T, db *sql.DB, eventID int64) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow("SELECT COUNT(*) FROM notifications WHERE event_id = :1", eventID).Scan(&n))
	return n
}

func TestOracleStoreIsIdempotentUnderConcurrentRedelivery(t *testing.T) {
	db := openDB(t)
	st := &notify.Oracle{DB: db}
	consumer := fmt.Sprintf("it-%d", time.Now().UnixNano())
	eventID := time.Now().UnixNano() % 1_000_000_000_000 // unique enough, fits NUMBER
	n := &notify.Notification{OrderID: "o-it", UserID: "u-it", Channel: "EMAIL", Text: "hello"}

	// 20 consumers receive the same event at the same moment.
	var firsts atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			processed, err := st.Process(context.Background(), consumer, eventID, n)
			assert.NoError(t, err)
			if processed {
				firsts.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	assert.EqualValues(t, 1, firsts.Load(), "exactly one delivery does the work")
	assert.Equal(t, 1, notificationCount(t, db, eventID), "exactly one notification row")
}

func TestOracleStoreRecordsEventsWithoutNotification(t *testing.T) {
	db := openDB(t)
	st := &notify.Oracle{DB: db}
	consumer := fmt.Sprintf("it-%d", time.Now().UnixNano())
	eventID := time.Now().UnixNano()%1_000_000_000_000 + 1

	processed, err := st.Process(context.Background(), consumer, eventID, nil)
	require.NoError(t, err)
	assert.True(t, processed)
	processed, err = st.Process(context.Background(), consumer, eventID, nil)
	require.NoError(t, err)
	assert.False(t, processed)
	assert.Zero(t, notificationCount(t, db, eventID))
}

func TestDifferentConsumersEachProcessTheSameEvent(t *testing.T) {
	db := openDB(t)
	st := &notify.Oracle{DB: db}
	eventID := time.Now().UnixNano()%1_000_000_000_000 + 2
	suffix := time.Now().UnixNano()

	a, err := st.Process(context.Background(), fmt.Sprintf("email-%d", suffix), eventID, nil)
	require.NoError(t, err)
	b, err := st.Process(context.Background(), fmt.Sprintf("audit-%d", suffix), eventID, nil)
	require.NoError(t, err)
	assert.True(t, a)
	assert.True(t, b, "the inbox key is (consumer, event): another consumer group is independent")
}

// Package publisher sends outbox events to Kafka.
package publisher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"

	"github.com/nhancaon/flashsale/services/outbox-worker/internal/outbox"
)

// Kafka publishes with kafka-go. Messages are keyed by aggregate id, so all events of one order go to one
// partition and keep their order.
type Kafka struct {
	writer *kafka.Writer
}

func NewKafka(brokers []string, topic string) *Kafka {
	return &Kafka{writer: &kafka.Writer{
		Addr:                   kafka.TCP(brokers...),
		Topic:                  topic,
		Balancer:               &kafka.Hash{},
		RequiredAcks:           kafka.RequireAll, // wait for the broker to persist before we mark SENT
		BatchTimeout:           5 * time.Millisecond,
		WriteTimeout:           5 * time.Second,
		ReadTimeout:            5 * time.Second,
		MaxAttempts:            1, // retries are the outbox's job (with backoff and attempt counting)
		AllowAutoTopicCreation: false,
	}}
}

// Publish writes the whole batch in one call and maps broker errors back to individual events.
func (k *Kafka) Publish(ctx context.Context, events []outbox.Event) []error {
	msgs := make([]kafka.Message, len(events))
	for i, e := range events {
		msgs[i] = kafka.Message{
			Key:   []byte(e.AggregateID),
			Value: []byte(e.Payload),
			Headers: []kafka.Header{
				{Key: "event-id", Value: []byte(strconv.FormatInt(e.ID, 10))},
				{Key: "event-type", Value: []byte(e.Type)},
			},
		}
	}
	results := make([]error, len(events))
	err := k.writer.WriteMessages(ctx, msgs...)
	if err == nil {
		return results
	}
	var perMessage kafka.WriteErrors
	if errors.As(err, &perMessage) && len(perMessage) == len(events) {
		copy(results, perMessage)
		return results
	}
	for i := range results { // unknown which ones failed: treat the whole batch as failed
		results[i] = err
	}
	return results
}

func (k *Kafka) Close() error { return k.writer.Close() }

// EnsureTopic creates the topic when it does not exist (idempotent).
func EnsureTopic(ctx context.Context, broker, topic string, partitions int) error {
	conn, err := (&kafka.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", broker)
	if err != nil {
		return fmt.Errorf("dial kafka: %w", err)
	}
	defer conn.Close()
	controller, err := conn.Controller()
	if err != nil {
		return fmt.Errorf("find controller: %w", err)
	}
	ctrl, err := kafka.DialContext(ctx, "tcp", net.JoinHostPort(controller.Host, strconv.Itoa(controller.Port)))
	if err != nil {
		return fmt.Errorf("dial controller: %w", err)
	}
	defer ctrl.Close()
	err = ctrl.CreateTopics(kafka.TopicConfig{Topic: topic, NumPartitions: partitions, ReplicationFactor: 1})
	if err != nil && !errors.Is(err, kafka.TopicAlreadyExists) {
		return fmt.Errorf("create topic %s: %w", topic, err)
	}
	return waitForLeaders(ctx, conn, topic, partitions)
}

// waitForLeaders blocks until every partition has an elected leader. Right after creation the first writes
// would otherwise fail with "leader not available" and burn delivery attempts.
func waitForLeaders(ctx context.Context, conn *kafka.Conn, topic string, partitions int) error {
	deadline := time.Now().Add(15 * time.Second)
	for {
		ps, err := conn.ReadPartitions(topic)
		ready := err == nil && len(ps) >= partitions
		for _, p := range ps {
			if p.Leader.ID < 0 || p.Leader.Host == "" {
				ready = false
			}
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("topic %s has no leaders after 15s (err=%v)", topic, err)
		}
		select {
		case <-time.After(200 * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

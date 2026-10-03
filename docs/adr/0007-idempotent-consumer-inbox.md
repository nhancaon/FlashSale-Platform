# ADR 0007: Idempotent consumer with an inbox table

Status: accepted (Phase 4)

## Context
Kafka delivery to the notification service is at-least-once: the outbox relay may publish an event twice after a crash
(ADR 0006), and a consumer may crash after handling a message but before committing its offset.
A customer must still get exactly one email per confirmed order.

## Decision
The **event id** (the outbox row id, sent in the `event-id` header) is the idempotency key.
In **one Oracle transaction** the consumer inserts `(consumer, event_id)` into `processed_events` (primary key) and then the
notification row. A duplicate insert (ORA-00001) means the event was already handled: nothing else happens and the offset
is committed. The effect and the dedupe record commit atomically, so there is no window where one exists without the other.

- Offsets are committed **after** the transaction, never before: a crash re-delivers the message and the inbox absorbs it.
- Infrastructure errors (database down) are retried with growing sleeps and the offset is not committed, so a message is
  never skipped. On shutdown mid-retry the offset stays uncommitted and the message comes back.
- A payload that can never be parsed ("poison") is recorded in the inbox and skipped, so it cannot block its partition.
  There is no dead-letter topic yet; the raw message stays in Kafka and the event row in the outbox.
- `ORDER_CREATED` is recorded but sends nothing; `ORDER_CONFIRMED` sends an email, `ORDER_FAILED` an SMS (simulated: a row in `notifications`).
- The key is `(consumer, event_id)`, so another consumer group can process the same event independently.

## Verified
20 concurrent deliveries of one event on real Oracle create exactly one notification row; loop tests (fake Kafka reader) cover
redelivery, DB outage with retry, and not committing unhandled messages; `make e2e-outbox` shows 150 notifications for 150 orders
with a worker killed during the run.

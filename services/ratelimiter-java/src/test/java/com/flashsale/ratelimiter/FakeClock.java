package com.flashsale.ratelimiter;

import java.time.Clock;
import java.time.Duration;
import java.time.Instant;
import java.time.ZoneId;
import java.time.ZoneOffset;
import java.util.concurrent.atomic.AtomicReference;

/** Controllable clock; the limiter passes its time to Redis as an argument. */
final class FakeClock extends Clock {

	// Aligned to a minute so fixed-window boundaries are predictable.
	private final AtomicReference<Instant> now = new AtomicReference<>(Instant.ofEpochSecond(1_700_000_040L));

	void advance(Duration d) {
		now.updateAndGet(t -> t.plus(d));
	}

	@Override
	public ZoneId getZone() {
		return ZoneOffset.UTC;
	}

	@Override
	public Clock withZone(ZoneId zone) {
		return this;
	}

	@Override
	public Instant instant() {
		return now.get();
	}
}

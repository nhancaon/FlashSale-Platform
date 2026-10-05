package lab;

import java.io.IOException;
import java.lang.management.ManagementFactory;
import java.lang.management.ThreadMXBean;
import java.nio.file.Files;
import java.nio.file.Path;
import java.security.MessageDigest;
import java.security.NoSuchAlgorithmException;
import java.time.Duration;
import java.util.ArrayList;
import java.util.Arrays;
import java.util.LinkedHashMap;
import java.util.List;
import java.util.Map;
import java.util.concurrent.ArrayBlockingQueue;
import java.util.concurrent.BlockingQueue;
import java.util.concurrent.CompletableFuture;
import java.util.concurrent.CountDownLatch;
import java.util.concurrent.ExecutorService;
import java.util.concurrent.Executors;
import java.util.concurrent.ForkJoinPool;
import java.util.concurrent.Future;
import java.util.concurrent.StructuredTaskScope;
import java.util.concurrent.SynchronousQueue;
import java.util.concurrent.TimeUnit;
import java.util.concurrent.atomic.AtomicInteger;
import java.util.concurrent.atomic.AtomicLong;
import java.util.concurrent.atomic.LongAdder;
import java.util.concurrent.locks.ReentrantLock;
import java.util.function.Supplier;

/**
 * Concurrency lab, Java side. Same flags and same JSON lines as ../go/main.go:
 * {@code java --enable-preview -jar lab.jar -exp 1 -variant virtual -n 100000 [-reps 5] [-warmup 1]}.
 */
public final class Main {

	private Main() {
	}

	private static final int CPUS = Runtime.getRuntime().availableProcessors();

	public static void main(String[] args) throws Exception {
		Map<String, String> a = new LinkedHashMap<>();
		for (int i = 0; i + 1 < args.length; i += 2) {
			a.put(args[i].replaceFirst("^-+", ""), args[i + 1]);
		}
		int exp = Integer.parseInt(a.getOrDefault("exp", "0"));
		String variant = a.getOrDefault("variant", "");
		int n = Integer.parseInt(a.getOrDefault("n", "0"));
		int reps = Integer.parseInt(a.getOrDefault("reps", "5"));
		int warmup = Integer.parseInt(a.getOrDefault("warmup", "1"));
		String p = String.valueOf(n);

		switch (exp) {
			case 1 -> measure(1, variant, p, warmup, reps, () -> exp1(variant, n));
			case 2 -> measure(2, variant, p, warmup, reps, () -> exp2(variant, n, 2000));
			case 3 -> measure(3, variant, p, warmup, reps, () -> exp3(variant, n));
			case 4 -> measure(4, variant, "100x" + p, warmup, reps, () -> exp4(variant, 100, n));
			case 5 -> measure(5, "unsynchronized-repeat", p, 0, reps, () -> exp5(n));
			case 6 -> measure(6, "blockingqueue-" + variant, p, warmup, reps, () -> exp6(variant, n, 100, 4, 4));
			case 7 -> measure(7, variant, p, warmup, reps, () -> exp7(variant, n));
			case 8 -> measure(8, variant, "", 0, reps, () -> exp8(variant));
			case 9 -> {
				List<String> in = lines(n); // input built once, outside the measured time
				measure(9, variant, p, warmup, reps, () -> exp9(variant, in));
			}
			default -> {
				System.err.println("lab: unknown experiment " + exp + " (10 is counted by report.mjs)");
				System.exit(2);
			}
		}
		System.exit(0); // experiment 8 leaves deadlocked threads behind on purpose
	}

	// ---------------------------------------------------------------- harness

	interface Exp {
		Map<String, Double> run() throws Exception;
	}

	static void measure(int exp, String variant, String param, int warmup, int reps, Exp fn) throws Exception {
		for (int i = 0; i < warmup; i++) {
			fn.run();
		}
		var os = (com.sun.management.OperatingSystemMXBean) ManagementFactory.getOperatingSystemMXBean();
		for (int rep = 1; rep <= reps; rep++) {
			System.gc();
			long cpu0 = os.getProcessCpuTime();
			long t0 = System.nanoTime();
			Map<String, Double> m = new LinkedHashMap<>(fn.run());
			long wall = System.nanoTime() - t0;
			long cpu = os.getProcessCpuTime() - cpu0;
			m.put("wallMs", wall / 1e6);
			m.put("cpuMs", cpu / 1e6);
			m.put("cpuUtil", (double) cpu / wall / CPUS);
			m.put("peakRssMb", peakRssMb());
			emit(exp, variant, param, rep, m);
		}
	}

	static void emit(int exp, String variant, String param, int rep, Map<String, Double> m) {
		StringBuilder sb = new StringBuilder();
		sb.append("{\"exp\":").append(exp).append(",\"lang\":\"java\",\"variant\":\"").append(variant)
			.append("\",\"param\":\"").append(param).append("\",\"rep\":").append(rep).append(",\"metrics\":{");
		int i = 0;
		for (var e : m.entrySet()) {
			sb.append(i++ > 0 ? "," : "").append('"').append(e.getKey()).append("\":").append(e.getValue());
		}
		System.out.println(sb.append("}}"));
	}

	/** VmHWM, the same source as the Go side. */
	static double peakRssMb() {
		try {
			for (String l : Files.readAllLines(Path.of("/proc/self/status"))) {
				if (l.startsWith("VmHWM:")) {
					return Double.parseDouble(l.substring(6).replace("kB", "").trim()) / 1024;
				}
			}
		}
		catch (IOException ex) {
			// not Linux: no value
		}
		return -1;
	}

	static double percentile(double[] sorted, double p) {
		int i = (int) (p / 100 * sorted.length + 0.5) - 1;
		return sorted[Math.max(0, Math.min(i, sorted.length - 1))];
	}

	static double b2d(boolean b) {
		return b ? 1 : 0;
	}

	// ---------------------------------------------------------------- 1. N tasks, each sleeps 100 ms

	static Map<String, Double> exp1(String variant, int n) throws InterruptedException {
		Thread.Builder builder = switch (variant) {
			case "platform" -> Thread.ofPlatform();
			case "virtual" -> Thread.ofVirtual();
			default -> throw new IllegalArgumentException(variant);
		};
		List<Thread> threads = new ArrayList<>(n);
		try {
			for (int i = 0; i < n; i++) {
				threads.add(builder.start(() -> {
					try {
						Thread.sleep(100);
					}
					catch (InterruptedException ex) {
						Thread.currentThread().interrupt();
					}
				}));
			}
		}
		catch (OutOfMemoryError ex) { // "unable to create native thread": the platform-thread limit
			for (Thread t : threads) {
				t.join();
			}
			return Map.of("tasks", (double) threads.size(), "crashed", 1.0);
		}
		for (Thread t : threads) {
			t.join();
		}
		return Map.of("tasks", (double) n, "crashed", 0.0);
	}

	// ---------------------------------------------------------------- 2. worker pool, CPU-bound jobs

	private static final ThreadLocal<MessageDigest> SHA = ThreadLocal.withInitial(() -> {
		try {
			return MessageDigest.getInstance("SHA-256");
		}
		catch (NoSuchAlgorithmException ex) {
			throw new IllegalStateException(ex);
		}
	});

	/** Same work as cpuJob in Go: hash a 32-byte buffer `rounds` times. */
	static byte cpuJob(int seed, int rounds) {
		MessageDigest md = SHA.get();
		byte[] buf = new byte[32];
		buf[0] = (byte) seed;
		for (int i = 0; i < rounds; i++) {
			buf = md.digest(buf);
		}
		return buf[0];
	}

	static Map<String, Double> exp2(String variant, int jobs, int rounds) throws Exception {
		LongAdder sink = new LongAdder();
		long t0 = System.nanoTime();
		switch (variant) {
			case "fixed-pool" -> {
				try (ExecutorService pool = Executors.newFixedThreadPool(CPUS)) {
					for (int j = 0; j < jobs; j++) {
						int job = j;
						pool.execute(() -> sink.add(cpuJob(job, rounds)));
					}
				} // close() waits for every job
			}
			case "forkjoin" -> {
				ForkJoinPool pool = new ForkJoinPool(CPUS);
				try {
					pool.submit(() -> java.util.stream.IntStream.range(0, jobs).parallel()
						.forEach(j -> sink.add(cpuJob(j, rounds)))).get();
				}
				finally {
					pool.shutdown();
				}
			}
			default -> throw new IllegalArgumentException(variant);
		}
		double secs = (System.nanoTime() - t0) / 1e9;
		return Map.of("jobsPerSec", jobs / secs, "workers", (double) CPUS);
	}

	// ---------------------------------------------------------------- 3. fan-out of simulated I/O calls

	/** Same deterministic latencies as Go: 50 + (i*7919 mod 151) ms. */
	static long latencyMs(int i) {
		return 50 + ((long) i * 7919) % 151;
	}

	static Map<String, Double> exp3(String variant, int n) throws Exception {
		double[] overhead = new double[n];
		long t0 = System.nanoTime();
		switch (variant) {
			case "virtual" -> { // blocking style, one cheap thread per call
				try (ExecutorService ex = Executors.newVirtualThreadPerTaskExecutor()) {
					for (int i = 0; i < n; i++) {
						int k = i;
						ex.submit(() -> {
							Thread.sleep(latencyMs(k));
							overhead[k] = (System.nanoTime() - t0) / 1e6 - latencyMs(k);
							return null;
						});
					}
				}
			}
			case "completablefuture" -> { // asynchronous style, no thread waits: a timer completes each call
				CompletableFuture<?>[] all = new CompletableFuture<?>[n];
				for (int i = 0; i < n; i++) {
					int k = i;
					all[i] = CompletableFuture.runAsync(
						() -> overhead[k] = (System.nanoTime() - t0) / 1e6 - latencyMs(k),
						CompletableFuture.delayedExecutor(latencyMs(k), TimeUnit.MILLISECONDS));
				}
				CompletableFuture.allOf(all).get(30, TimeUnit.SECONDS);
			}
			default -> throw new IllegalArgumentException(variant);
		}
		Arrays.sort(overhead);
		return Map.of("calls", (double) n, "overheadP50Ms", percentile(overhead, 50), "overheadP99Ms", percentile(overhead, 99));
	}

	// ---------------------------------------------------------------- 4. shared counter

	static Map<String, Double> exp4(String variant, int threads, int perThread) throws InterruptedException {
		long expected = (long) threads * perThread;
		Supplier<Long> result;
		Runnable inc;
		switch (variant) {
			case "synchronized" -> {
				long[] c = new long[1];
				Object lock = new Object();
				inc = () -> {
					synchronized (lock) {
						c[0]++;
					}
				};
				result = () -> c[0];
			}
			case "reentrantlock" -> {
				long[] c = new long[1];
				ReentrantLock lock = new ReentrantLock();
				inc = () -> {
					lock.lock();
					try {
						c[0]++;
					}
					finally {
						lock.unlock();
					}
				};
				result = () -> c[0];
			}
			case "atomiclong" -> {
				AtomicLong c = new AtomicLong();
				inc = c::incrementAndGet;
				result = c::get;
			}
			case "longadder" -> {
				LongAdder c = new LongAdder();
				inc = c::increment;
				result = c::sum;
			}
			default -> throw new IllegalArgumentException(variant);
		}
		long t0 = System.nanoTime();
		Thread[] ts = new Thread[threads];
		for (int i = 0; i < threads; i++) {
			ts[i] = Thread.ofPlatform().start(() -> {
				for (int k = 0; k < perThread; k++) {
					inc.run();
				}
			});
		}
		for (Thread t : ts) {
			t.join();
		}
		long el = System.nanoTime() - t0;
		return Map.of("nsPerOp", (double) el / expected, "correct", b2d(result.get() == expected));
	}

	// ---------------------------------------------------------------- 5. race on purpose (no detector in the JDK)

	/** Runs the racy counter `runs` times and reports in how many runs updates were lost: detection by repetition. */
	static Map<String, Double> exp5(int runs) throws InterruptedException {
		int lossy = 0;
		long lost = 0;
		for (int r = 0; r < runs; r++) {
			int[] c = new int[1];
			Thread[] ts = new Thread[8];
			for (int i = 0; i < ts.length; i++) {
				ts[i] = Thread.ofPlatform().start(() -> {
					for (int k = 0; k < 10_000; k++) {
						c[0]++; // the same bug as race/counter.go
					}
				});
			}
			for (Thread t : ts) {
				t.join();
			}
			if (c[0] != 80_000) {
				lossy++;
				lost += 80_000 - c[0];
			}
		}
		return Map.of("runs", (double) runs, "runsWithLostUpdates", (double) lossy, "lostUpdates", (double) lost,
			"detected", b2d(lossy > 0));
	}

	// ---------------------------------------------------------------- 6. bounded producer-consumer

	static Map<String, Double> exp6(String variant, int items, int capacity, int producers, int consumers) throws Exception {
		BlockingQueue<Integer> q = new ArrayBlockingQueue<>(capacity);
		AtomicLong produced = new AtomicLong();
		AtomicLong dropped = new AtomicLong();
		AtomicLong consumed = new AtomicLong();
		AtomicLong blockedNs = new AtomicLong();
		Integer poison = -1;
		long t0 = System.nanoTime();
		List<Thread> cs = new ArrayList<>();
		for (int c = 0; c < consumers; c++) {
			cs.add(Thread.ofPlatform().start(() -> {
				try {
					for (Integer v = q.take(); !v.equals(poison); v = q.take()) {
						cpuJob(v, 2);
						consumed.incrementAndGet();
					}
				}
				catch (InterruptedException ex) {
					Thread.currentThread().interrupt();
				}
			}));
		}
		List<Thread> ps = new ArrayList<>();
		for (int p = 0; p < producers; p++) {
			int first = p;
			ps.add(Thread.ofPlatform().start(() -> {
				try {
					for (int i = first; i < items; i += producers) {
						if (variant.equals("block")) {
							long t = System.nanoTime();
							q.put(i);
							blockedNs.addAndGet(System.nanoTime() - t);
						}
						else if (!q.offer(i)) {
							dropped.incrementAndGet();
							continue;
						}
						produced.incrementAndGet();
					}
				}
				catch (InterruptedException ex) {
					Thread.currentThread().interrupt();
				}
			}));
		}
		for (Thread t : ps) {
			t.join();
		}
		for (int c = 0; c < consumers; c++) {
			q.put(poison);
		}
		for (Thread t : cs) {
			t.join();
		}
		double secs = (System.nanoTime() - t0) / 1e9;
		return Map.of("itemsPerSec", consumed.get() / secs, "dropped", (double) dropped.get(),
			"producerBlockedMs", blockedNs.get() / 1e6 / producers, "correct", b2d(consumed.get() == produced.get()));
	}

	// ---------------------------------------------------------------- 7. cancellation and timeout

	/** A long job made of 10 ms steps; it stops when interrupted (Java's cancellation signal). */
	static Void steps(CountDownLatch exited) throws InterruptedException {
		try {
			while (true) {
				Thread.sleep(10);
			}
		}
		finally {
			exited.countDown();
		}
	}

	static Map<String, Double> exp7(String variant, int tasks) throws Exception {
		CountDownLatch exited = new CountDownLatch(tasks);
		long cancelAt;
		switch (variant) {
			case "future-cancel" -> {
				ExecutorService ex = Executors.newVirtualThreadPerTaskExecutor();
				List<Future<Void>> fs = new ArrayList<>(tasks);
				for (int i = 0; i < tasks; i++) {
					fs.add(ex.submit(() -> steps(exited)));
				}
				Thread.sleep(100);
				cancelAt = System.nanoTime();
				fs.forEach(f -> f.cancel(true)); // interrupts the running threads
				ex.shutdown();
			}
			case "structured" -> {
				cancelAt = System.nanoTime() + 100_000_000L; // the scope deadline below
				try (var scope = StructuredTaskScope.open(StructuredTaskScope.Joiner.<Void>awaitAll(),
						cf -> cf.withTimeout(Duration.ofMillis(100)))) {
					for (int i = 0; i < tasks; i++) {
						scope.fork(() -> steps(exited));
					}
					try {
						scope.join(); // the timeout cancels every subtask; close() waits until they all ended
					}
					catch (StructuredTaskScope.TimeoutException expected) {
						// the 100 ms deadline: what we want
					}
				}
			}
			default -> throw new IllegalArgumentException(variant);
		}
		boolean all = exited.await(5, TimeUnit.SECONDS);
		double stopMs = Math.max(0, (System.nanoTime() - cancelAt) / 1e6); // from the cancel/deadline until all ended
		Thread.sleep(50);
		return Map.of("stopMs", stopMs, "leaked", (double) exited.getCount(),
			"allStopped", b2d(all));
	}

	// ---------------------------------------------------------------- 8. deadlock and leak, then diagnosis

	static Map<String, Double> exp8(String variant) throws Exception {
		ThreadMXBean mx = ManagementFactory.getThreadMXBean();
		switch (variant) {
			case "deadlock" -> {
				Object a = new Object();
				Object b = new Object();
				long t0 = System.nanoTime();
				Thread.ofPlatform().daemon().start(() -> lockBoth(a, b));
				Thread.ofPlatform().daemon().start(() -> lockBoth(b, a));
				// The JVM finds lock cycles itself (the same check as jstack's "Found one Java-level deadlock").
				for (int i = 0; i < 200; i++) {
					long[] ids = mx.findDeadlockedThreads();
					if (ids != null) {
						return Map.of("detected", 1.0, "detectMs", (System.nanoTime() - t0) / 1e6,
							"stuckThreads", (double) ids.length);
					}
					Thread.sleep(5);
				}
				return Map.of("detected", 0.0, "detectMs", (System.nanoTime() - t0) / 1e6, "stuckThreads", 0.0);
			}
			case "leak" -> { // same bug as Go: a result handed over to a receiver that gave up
				int before = mx.getThreadCount();
				for (int i = 0; i < 1000; i++) {
					SynchronousQueue<Integer> ch = new SynchronousQueue<>();
					Thread.ofPlatform().daemon().start(() -> {
						try {
							ch.put(1);
						}
						catch (InterruptedException ex) {
							Thread.currentThread().interrupt();
						}
					});
					ch.poll(1, TimeUnit.MICROSECONDS);
				}
				Thread.sleep(20);
				int leaked = mx.getThreadCount() - before;
				return Map.of("detected", b2d(leaked > 0), "leaked", (double) leaked);
			}
			default -> throw new IllegalArgumentException(variant);
		}
	}

	static void lockBoth(Object first, Object second) {
		synchronized (first) {
			try {
				Thread.sleep(10);
			}
			catch (InterruptedException ex) {
				Thread.currentThread().interrupt();
			}
			synchronized (second) {
				first.hashCode();
			}
		}
	}

	// ---------------------------------------------------------------- 9. pipeline parse -> transform -> write

	record Rec(int id, String sku, int qty, int total) {
	}

	static List<String> lines(int n) {
		List<String> out = new ArrayList<>(n);
		for (int i = 0; i < n; i++) {
			out.add(i + ",item-" + (i % 1000) + "," + (i % 97));
		}
		return out;
	}

	static Rec parse(String l) {
		String[] p = l.split(",");
		return p.length == 3 ? new Rec(Integer.parseInt(p[0]), p[1], Integer.parseInt(p[2]), 0) : null;
	}

	static Map<String, Double> exp9(String variant, List<String> in) throws Exception {
		LongAdder written = new LongAdder();
		LongAdder checksum = new LongAdder();
		switch (variant) {
			case "stream" -> in.stream().map(Main::parse).filter(r -> r != null)
				.map(r -> new Rec(r.id(), r.sku(), r.qty(), r.qty() * 490))
				.forEach(r -> {
					written.increment();
					checksum.add(r.total());
				});
			case "parallel-stream" -> in.parallelStream().map(Main::parse).filter(r -> r != null)
				.map(r -> new Rec(r.id(), r.sku(), r.qty(), r.qty() * 490))
				.forEach(r -> {
					written.increment();
					checksum.add(r.total());
				});
			case "queue-virtual" -> { // the Go design: stages connected by bounded queues, threads per stage
				Rec end = new Rec(-1, "", 0, 0);
				BlockingQueue<Rec> parsed = new ArrayBlockingQueue<>(1024);
				BlockingQueue<Rec> transformed = new ArrayBlockingQueue<>(1024);
				AtomicInteger transformers = new AtomicInteger(CPUS);
				try (ExecutorService ex = Executors.newVirtualThreadPerTaskExecutor()) {
					ex.submit(() -> {
						for (String l : in) {
							Rec r = parse(l);
							if (r != null) {
								parsed.put(r);
							}
						}
						for (int i = 0; i < CPUS; i++) {
							parsed.put(end);
						}
						return null;
					});
					for (int w = 0; w < CPUS; w++) {
						ex.submit(() -> {
							for (Rec r = parsed.take(); r != end; r = parsed.take()) {
								transformed.put(new Rec(r.id(), r.sku(), r.qty(), r.qty() * 490));
							}
							if (transformers.decrementAndGet() == 0) {
								transformed.put(end);
							}
							return null;
						});
					}
					for (Rec r = transformed.take(); r != end; r = transformed.take()) {
						written.increment();
						checksum.add(r.total());
					}
				}
			}
			default -> throw new IllegalArgumentException(variant);
		}
		return Map.of("written", written.doubleValue(), "checksum", checksum.doubleValue());
	}
}

// Generates deploy/compose/grafana/dashboards/flashsale.json (the committed file is what Grafana provisions).
//   node scripts/gen-dashboard.mjs
// Go services expose http_requests_total / http_request_duration_seconds, Java services expose the Micrometer names
// http_server_requests_seconds_*; every RED panel therefore has one query per runtime.
import { writeFileSync, mkdirSync } from "node:fs";
import { dirname } from "node:path";
import { fileURLToPath } from "node:url";

const DS = { type: "prometheus", uid: "prometheus" };
let nextId = 1;
const panels = [];

function row(title, y) {
  panels.push({ id: nextId++, type: "row", title, collapsed: false, gridPos: { h: 1, w: 24, x: 0, y }, panels: [] });
}

function ts(title, targets, { x, y, w = 8, h = 8, unit = "short", desc = "", stack = false, min } = {}) {
  panels.push({
    id: nextId++, type: "timeseries", title, description: desc, datasource: DS,
    gridPos: { h, w, x, y },
    targets: targets.map((t, i) => ({ refId: String.fromCharCode(65 + i), datasource: DS, expr: t[0], legendFormat: t[1], range: true })),
    fieldConfig: { defaults: { unit, min, custom: { lineWidth: 2, fillOpacity: stack ? 30 : 8, stacking: { mode: stack ? "normal" : "none" }, showPoints: "never" } }, overrides: [] },
    options: { legend: { displayMode: "table", placement: "bottom", calcs: ["lastNotNull", "max"] }, tooltip: { mode: "multi", sort: "desc" } },
  });
}

function stat(title, expr, { x, y, w = 3, h = 4, unit = "short", legend = "" } = {}) {
  panels.push({
    id: nextId++, type: "stat", title, datasource: DS, gridPos: { h, w, x, y },
    targets: [{ refId: "A", datasource: DS, expr, legendFormat: legend, instant: true }],
    fieldConfig: { defaults: { unit, thresholds: { mode: "absolute", steps: [{ color: "red", value: null }, { color: "green", value: 1 }] }, mappings: [] }, overrides: [] },
    options: { reduceOptions: { calcs: ["lastNotNull"] }, colorMode: "background", graphMode: "none", textMode: "value_and_name" },
  });
}

// ---- Status row: which services are up ----
row("Status", 0);
const services = ["gateway", "order", "inventory-go", "inventory-java", "notification", "outbox-worker", "ratelimiter-go", "ratelimiter-java"];
services.forEach((s, i) => stat(s, `max(up{job="${s}"}) or vector(0)`, { x: i * 3, y: 1, legend: s }));

// ---- RED ----
row("Traffic, errors, latency (RED)", 5);
const goHttp = (m) => m;
ts("Requests per second", [
  [`sum by (job) (rate(http_requests_total[1m]))`, "{{job}}"],
  [`sum by (job) (rate(http_server_requests_seconds_count{uri!~"/metrics|/healthz|/readyz"}[1m]))`, "{{job}}"],
], { x: 0, y: 6, unit: "reqps", desc: "Go services (http_requests_total) and Java services (Micrometer) side by side." });
ts("5xx errors per second", [
  [`sum by (job) (rate(http_requests_total{status=~"5.."}[1m]))`, "{{job}}"],
  [`sum by (job) (rate(http_server_requests_seconds_count{status=~"5.."}[1m]))`, "{{job}}"],
], { x: 8, y: 6, unit: "reqps", min: 0 });
ts("p95 latency", [
  [`histogram_quantile(0.95, sum by (le, job) (rate(http_request_duration_seconds_bucket{route!~"/metrics|/healthz|/readyz"}[1m])))`, "{{job}}"],
  [`histogram_quantile(0.95, sum by (le, job) (rate(http_server_requests_seconds_bucket{uri!~"/metrics|/healthz|/readyz"}[1m])))`, "{{job}}"],
], { x: 16, y: 6, unit: "s", desc: "Includes the time a request waits for a database connection or a row lock." });

// ---- Business ----
row("Flash sale", 14);
ts("Reserve results (inventory)", [
  [`sum by (job, result) (rate(inventory_reserve_total[1m]))`, "{{job}} {{result}}"],
], { x: 0, y: 15, unit: "ops", stack: true });
ts("Orders by result (order service)", [
  [`sum by (result) (rate(order_requests_total[1m]))`, "{{result}}"],
], { x: 8, y: 15, unit: "ops", stack: true });
ts("Gateway rejections (429) and limiter errors", [
  [`sum by (scope) (rate(gateway_ratelimit_rejected_total[1m]))`, "429 {{scope}}"],
  [`sum(rate(gateway_ratelimit_errors_total[1m]))`, "limiter errors"],
], { x: 16, y: 15, unit: "ops" });

// ---- Messaging ----
row("Outbox and notifications", 23);
ts("Outbox backlog (NEW events)", [[`max(outbox_pending_events)`, "pending"]], { x: 0, y: 24, desc: "Relay lag: events written by orders and not yet published to Kafka." });
ts("Outbox published per second", [
  [`sum(rate(outbox_published_total[1m]))`, "published"],
  [`sum(rate(outbox_failed_attempts_total[1m]))`, "failed attempts"],
], { x: 8, y: 24, unit: "ops" });
ts("Notifications by result", [[`sum by (result) (rate(notification_processed_total[1m]))`, "{{result}}"]], { x: 16, y: 24, unit: "ops", stack: true });

// ---- Resilience ----
row("Resilience", 32);
ts("Circuit breakers (1 = open)", [
  [`gateway_upstream_breaker_state == bool 2`, "gateway -> {{upstream}}"],
  [`resilience4j_circuitbreaker_state{state="open"}`, "order -> {{name}}"],
], { x: 0, y: 33, min: 0, desc: "gateway: gobreaker, order: Resilience4j." });
ts("Rate limiter decisions", [
  [`sum by (job) (rate(ratelimit_allowed_total[1m]))`, "{{job}} allowed"],
  [`sum by (job) (rate(ratelimit_rejected_total[1m]))`, "{{job}} rejected"],
], { x: 8, y: 33, unit: "ops" });
ts("Inventory cache hit ratio", [
  [`sum by (job) (rate(inventory_cache_total{result="hit"}[1m])) / clamp_min(sum by (job) (rate(inventory_cache_total[1m])), 1e-9)`, "{{job}}"],
], { x: 16, y: 33, unit: "percentunit", min: 0 });

// ---- Resources ----
row("Resources", 41);
ts("Database connections in use", [
  [`db_pool_in_use_connections`, "{{job}} (Go)"],
  [`hikaricp_connections_active`, "{{job}} (Hikari)"],
  [`hikaricp_connections_pending`, "{{job}} waiting (Hikari)"],
], { x: 0, y: 42, min: 0, desc: "Pool size is 20 by default; sitting at the cap with waiters means the database is the bottleneck." });
ts("Go: pool waits per second", [[`rate(db_pool_wait_count_total[1m])`, "{{job}}"]], { x: 8, y: 42, unit: "ops", min: 0 });
ts("CPU cores used", [
  [`rate(process_cpu_seconds_total[1m])`, "{{job}} (Go)"],
  [`process_cpu_usage * system_cpu_count`, "{{job}} (JVM)"],
], { x: 16, y: 42, min: 0 });
ts("Memory in use", [
  [`go_memstats_heap_inuse_bytes`, "{{job}} Go heap"],
  [`sum by (job) (jvm_memory_used_bytes{area="heap"})`, "{{job}} JVM heap"],
], { x: 0, y: 50, unit: "bytes", min: 0 });
ts("Goroutines / JVM threads", [
  [`go_goroutines`, "{{job}} goroutines"],
  [`jvm_threads_live_threads`, "{{job}} threads"],
], { x: 8, y: 50, min: 0 });
ts("GC time (fraction of wall time)", [
  [`rate(go_gc_duration_seconds_sum[1m])`, "{{job}} (Go)"],
  [`sum by (job) (rate(jvm_gc_pause_seconds_sum[1m]))`, "{{job}} (JVM)"],
], { x: 16, y: 50, unit: "percentunit", min: 0 });

const dashboard = {
  uid: "flashsale-overview", title: "FlashSale overview", tags: ["flashsale"], timezone: "browser",
  schemaVersion: 39, version: 1, editable: true, refresh: "5s", time: { from: "now-15m", to: "now" },
  templating: { list: [] }, annotations: { list: [] }, panels,
};

const out = fileURLToPath(new URL("../deploy/compose/grafana/dashboards/flashsale.json", import.meta.url)); // decodes %20 in paths with spaces
mkdirSync(dirname(out), { recursive: true });
writeFileSync(out, JSON.stringify(dashboard, null, 2) + "\n");
console.log(`wrote ${panels.length} panels to ${out}`);

// Aggregates loadtest/results/phase6 (written by run-benchmark.sh) into:
//   loadtest/results/phase6/summary.json   every number used by the report
//   loadtest/results/phase6/summary.md     tables (median of the rounds, min-max in brackets)
//   docs/img/benchmark.svg                 small multiples, one panel per measure, Go vs Java
//   node loadtest/aggregate.mjs [results-dir]
import { readFileSync, writeFileSync, readdirSync, existsSync, mkdirSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = fileURLToPath(new URL("..", import.meta.url));
const dir = process.argv[2] ?? join(root, "loadtest/results/phase6");
const runsDir = join(dir, "runs");

const median = (a) => { const s = [...a].sort((x, y) => x - y); const m = s.length >> 1; return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2; };
const stats = (a) => ({ median: median(a), min: Math.min(...a), max: Math.max(...a), values: a });
const mib = (s) => { const [n, u] = [parseFloat(s), s.replace(/[\d.]/g, "")]; return u.startsWith("GiB") ? n * 1024 : u.startsWith("KiB") ? n / 1024 : u.startsWith("kB") ? n / 1024 : u.startsWith("MB") ? n : n; };

// ---- per run ----
const runs = [];
for (const f of readdirSync(runsDir).filter((f) => /^(go|java)-r\d+\.meta\.json$/.test(f))) {
  const name = f.replace(".meta.json", "");
  const meta = JSON.parse(readFileSync(join(runsDir, f), "utf8"));
  const k6 = JSON.parse(readFileSync(join(runsDir, `${name}.k6.json`), "utf8")).metrics;
  const rec = existsSync(join(runsDir, `${name}.reconcile.json`)) ? JSON.parse(readFileSync(join(runsDir, `${name}.reconcile.json`), "utf8")) : { status: "MISSING" };
  const d = k6["http_req_duration{name:POST /api/orders}"];
  const c = (k) => k6[k]?.count ?? 0;
  const answered = c("orders_created") + c("orders_replayed") + c("orders_sold_out") + c("orders_throttled") + c("orders_in_progress") + c("orders_unexpected");
  const seconds = k6.http_reqs.count / k6.http_reqs.rate;

  // docker stats samples: ts,name,cpu%,"used / limit"
  const csv = readFileSync(join(dir, "stats", `${name}.csv`), "utf8").trim().split("\n").filter(Boolean);
  const res = {};
  for (const line of csv) {
    const [, cname, cpu, mem] = line.split(",");
    const key = cname.replace(/^flashsale-/, "").replace(/-1$/, "").replace(/^inventory-(go|java)$/, "inventory");
    (res[key] ??= { cpu: [], mem: [] });
    res[key].cpu.push(parseFloat(cpu));
    res[key].mem.push(mib(mem.split("/")[0].trim()));
  }
  const busy = (a) => { const b = a.filter((x) => x > 5); return b.length ? b.reduce((s, x) => s + x, 0) / b.length : 0; };

  runs.push({
    name, impl: meta.impl, round: meta.round, meta, reconcile: rec,
    ordersPerSec: answered / seconds,
    p50: d.med, p95: d["p(95)"], p99: d["p(99)"], max: d.max,
    created: c("orders_created"), replayed: c("orders_replayed"), soldOut: c("orders_sold_out"),
    throttled: c("orders_throttled"), unexpected: c("orders_unexpected"), k6Exit: meta.k6Exit,
    invCpuBusy: busy(res.inventory?.cpu ?? []), invCpuPeak: Math.max(0, ...(res.inventory?.cpu ?? [0])),
    invMemPeak: Math.max(0, ...(res.inventory?.mem ?? [0])),
    orderCpuBusy: busy(res.order?.cpu ?? []), oracleCpuBusy: busy(res.oracle?.cpu ?? []),
  });
}
runs.sort((a, b) => a.name.localeCompare(b.name));

const staticFacts = existsSync(join(dir, "static.jsonl"))
  ? readFileSync(join(dir, "static.jsonl"), "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l)) : [];

const impls = ["go", "java"];
const measures = [
  { key: "ordersPerSec", label: "Orders answered per second", unit: "req/s", digits: 0, better: "higher" },
  { key: "p50", label: "Order latency p50", unit: "ms", digits: 1, better: "lower" },
  { key: "p95", label: "Order latency p95", unit: "ms", digits: 0, better: "lower" },
  { key: "p99", label: "Order latency p99", unit: "ms", digits: 0, better: "lower" },
  { key: "invCpuBusy", label: "Inventory CPU while busy", unit: "% of 1 core", digits: 0, better: "lower" },
  { key: "invMemPeak", label: "Inventory peak memory", unit: "MiB", digits: 0, better: "lower" },
];
const byImpl = Object.fromEntries(impls.map((i) => [i, runs.filter((r) => r.impl === i)]));
const summary = { generated: new Date().toISOString(), runs, staticFacts, measures: {} };
for (const m of measures) {
  summary.measures[m.key] = Object.fromEntries(impls.map((i) => [i, byImpl[i].length ? stats(byImpl[i].map((r) => r[m.key])) : null]));
}
const st = Object.fromEntries(staticFacts.map((s) => [s.impl, s]));
summary.measures.startupMs = Object.fromEntries(impls.map((i) => [i, st[i] ? stats(st[i].startupMs) : null]));
summary.measures.imageMiB = Object.fromEntries(impls.map((i) => [i, st[i] ? stats([st[i].imageBytes / 1048576]) : null]));
summary.measures.idleMiB = Object.fromEntries(impls.map((i) => [i, st[i] ? stats([mib(st[i].idleMem)]) : null]));
writeFileSync(join(dir, "summary.json"), JSON.stringify(summary, null, 2) + "\n");

// ---- markdown ----
const fmt = (s, d) => (s ? `${s.median.toFixed(d)} [${s.min.toFixed(d)}–${s.max.toFixed(d)}]` : "n/a");
let md = `| Measure | inventory-go | inventory-java |\n|---|---:|---:|\n`;
for (const m of measures) md += `| ${m.label} (${m.unit}, ${m.better} is better) | ${fmt(summary.measures[m.key].go, m.digits)} | ${fmt(summary.measures[m.key].java, m.digits)} |\n`;
md += `| Cold start to ready (ms, 5 restarts) | ${fmt(summary.measures.startupMs.go, 0)} | ${fmt(summary.measures.startupMs.java, 0)} |\n`;
md += `| Image size (MiB) | ${fmt(summary.measures.imageMiB.go, 1)} | ${fmt(summary.measures.imageMiB.java, 1)} |\n`;
md += `| Idle memory after start (MiB) | ${fmt(summary.measures.idleMiB.go, 1)} | ${fmt(summary.measures.idleMiB.java, 1)} |\n`;
md += `\n| Run | orders/s | p50 ms | p95 ms | p99 ms | created | sold out | replays | 429 | unexpected | reconcile | inventory CPU % | order CPU % | Oracle CPU % |\n|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|---|---:|---:|---:|\n`;
for (const r of runs) {
  md += `| ${r.name} | ${r.ordersPerSec.toFixed(0)} | ${r.p50.toFixed(1)} | ${r.p95.toFixed(0)} | ${r.p99.toFixed(0)} | ${r.created} | ${r.soldOut} | ${r.replayed} | ${r.throttled} | ${r.unexpected} | ${r.reconcile.status} | ${r.invCpuBusy.toFixed(0)} | ${r.orderCpuBusy.toFixed(0)} | ${r.oracleCpuBusy.toFixed(0)} |\n`;
}
writeFileSync(join(dir, "summary.md"), md);

// ---- SVG small multiples ----
const panels = [...measures, { key: "startupMs", label: "Cold start to ready", unit: "ms" }, { key: "imageMiB", label: "Image size", unit: "MiB" }];
const W = 300, H = 220, cols = 4, gap = 16, top = 56;
const rows = Math.ceil(panels.length / cols);
const width = cols * W + (cols - 1) * gap, height = top + rows * (H + gap);
const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;");
const nice = (v) => { if (v <= 0) return 1; const p = 10 ** Math.floor(Math.log10(v)); return [1, 2, 2.5, 5, 10].map((k) => k * p).find((x) => x >= v); };
const label = (v) => (v >= 100 ? v.toFixed(0) : v >= 10 ? v.toFixed(1) : v.toFixed(2));
let body = "";
panels.forEach((p, i) => {
  const x0 = (i % cols) * (W + gap), y0 = top + Math.floor(i / cols) * (H + gap);
  const data = impls.map((impl) => ({ impl, s: summary.measures[p.key][impl] }));
  const maxV = nice(Math.max(...data.map((d) => (d.s ? d.s.max : 0))) * 1.12);
  const plotL = x0 + 44, plotR = x0 + W - 12, plotT = y0 + 34, plotB = y0 + H - 26;
  const y = (v) => plotB - (v / maxV) * (plotB - plotT);
  body += `<g><text class="t" x="${x0}" y="${y0 + 14}">${esc(p.label)}</text><text class="m" x="${x0}" y="${y0 + 28}">${esc(p.unit)}${p.better ? ` · ${p.better} is better` : ""}</text>`;
  for (const f of [0, 0.5, 1]) {
    const v = maxV * f;
    body += `<line class="g" x1="${plotL}" x2="${plotR}" y1="${y(v)}" y2="${y(v)}"/><text class="a" x="${plotL - 6}" y="${y(v) + 4}" text-anchor="end">${label(v)}</text>`;
  }
  const bw = 54, slot = (plotR - plotL) / 2;
  data.forEach((d, k) => {
    if (!d.s) return;
    const cx = plotL + slot * k + slot / 2, bx = cx - bw / 2, by = y(d.s.median), bh = plotB - by;
    // Bar with a 4 px rounded top anchored to the baseline.
    const r = Math.min(4, bh);
    body += `<path class="s${k + 1}" d="M${bx},${plotB} V${by + r} Q${bx},${by} ${bx + r},${by} H${bx + bw - r} Q${bx + bw},${by} ${bx + bw},${by + r} V${plotB} Z"><title>${d.impl}: median ${label(d.s.median)} ${p.unit} (min ${label(d.s.min)}, max ${label(d.s.max)}, n=${d.s.values.length})</title></path>`;
    if (d.s.values.length > 1 && d.s.max > d.s.min) {
      body += `<line class="w" x1="${cx}" x2="${cx}" y1="${y(d.s.max)}" y2="${y(d.s.min)}"/><line class="w" x1="${cx - 8}" x2="${cx + 8}" y1="${y(d.s.max)}" y2="${y(d.s.max)}"/><line class="w" x1="${cx - 8}" x2="${cx + 8}" y1="${y(d.s.min)}" y2="${y(d.s.min)}"/>`;
    }
    body += `<text class="v" x="${cx}" y="${Math.min(y(d.s.max), by) - 6}" text-anchor="middle">${label(d.s.median)}</text>`;
    body += `<text class="a" x="${cx}" y="${plotB + 16}" text-anchor="middle">${d.impl}</text>`;
  });
  body += `<line class="b" x1="${plotL}" x2="${plotR}" y1="${plotB}" y2="${plotB}"/></g>`;
});
const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${width} ${height}" width="${width}" height="${height}" role="img" aria-label="inventory-go versus inventory-java benchmark, median of rounds with min-max whiskers">
<style>
  svg { --bg:#fcfcfb; --ink:#0b0b0b; --ink2:#52514e; --grid:#e6e5e0; --s1:#2a78d6; --s2:#eb6834; font-family: system-ui, -apple-system, "Segoe UI", sans-serif; }
  @media (prefers-color-scheme: dark) { svg { --bg:#1a1a19; --ink:#ffffff; --ink2:#c3c2b7; --grid:#33332f; --s1:#3987e5; --s2:#d95926; } }
  .bg { fill: var(--bg); } .t { fill: var(--ink); font-size: 14px; font-weight: 600; } .m, .a { fill: var(--ink2); font-size: 11px; }
  .v { fill: var(--ink); font-size: 12px; font-weight: 600; } .g { stroke: var(--grid); stroke-width: 1; } .b { stroke: var(--ink2); stroke-width: 1; }
  .w { stroke: var(--ink); stroke-width: 1.5; } .s1 { fill: var(--s1); } .s2 { fill: var(--s2); } .lg { font-size: 12px; fill: var(--ink); }
</style>
<rect class="bg" width="100%" height="100%"/>
<g><rect x="0" y="6" width="12" height="12" rx="2" class="s1"/><text class="lg" x="18" y="16">inventory-go</text>
<rect x="120" y="6" width="12" height="12" rx="2" class="s2"/><text class="lg" x="138" y="16">inventory-java</text>
<text class="m" x="260" y="16">bar = median of rounds, whisker = min–max</text></g>
${body}
</svg>
`;
const img = join(root, "docs/img/benchmark.svg");
mkdirSync(dirname(img), { recursive: true });
writeFileSync(img, svg);
console.log(md);
console.log(`runs: ${runs.length}, reconcile: ${runs.map((r) => `${r.name}=${r.reconcile.status}`).join(" ")}`);

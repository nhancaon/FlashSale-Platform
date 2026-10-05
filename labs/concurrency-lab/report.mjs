// Builds docs/concurrency-comparison.md from labs/concurrency-lab/results (make lab-report).
// The narrative lives in report-template.md; this script fills its placeholders:
//   {{env}}  {{table:N}}  {{chart:N}}  {{loc:N}}  {{loc-summary}}  {{exp10}}  {{flame:<profile>}}
// Charts and flame graphs are written to docs/img/lab-*.svg.
import { readFileSync, writeFileSync, readdirSync, existsSync, mkdirSync } from "node:fs";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { flameFromFile } from "./flame.mjs";

const here = fileURLToPath(new URL(".", import.meta.url));
const root = join(here, "..", "..");
const results = join(here, "results");
const img = join(root, "docs", "img");
mkdirSync(img, { recursive: true });

// ---------------------------------------------------------------- data

const rows = [];
for (const f of readdirSync(results).filter((f) => f.endsWith(".jsonl"))) {
  for (const line of readFileSync(join(results, f), "utf8").split("\n")) {
    if (line.startsWith("{")) rows.push(JSON.parse(line)); // JFR logs its startup on stdout: skip it
  }
}
for (const r of rows) { // derived metric: pipeline throughput
  if (r.exp === 9 && r.metrics.written && r.metrics.wallMs) r.metrics.linesPerSec = r.metrics.written / (r.metrics.wallMs / 1000);
}

const median = (a) => { const s = [...a].sort((x, y) => x - y); const m = s.length >> 1; return s.length % 2 ? s[m] : (s[m - 1] + s[m]) / 2; };
const stddev = (a) => { const mu = a.reduce((s, x) => s + x, 0) / a.length; return Math.sqrt(a.reduce((s, x) => s + (x - mu) ** 2, 0) / a.length); };

/** groups[exp] = [{lang, variant, param, metrics: {name: [values]}, crashed, notes}] in file order */
const groups = {};
for (const r of rows) {
  const list = (groups[r.exp] ??= []);
  let g = list.find((x) => x.lang === r.lang && x.variant === r.variant && x.param === r.param);
  if (!g) list.push((g = { lang: r.lang, variant: r.variant, param: r.param, metrics: {}, crashed: false, notes: "" }));
  if (r.metrics.crashed === 1 && r.rep === 0) { g.crashed = true; g.notes = r.notes ?? ""; g.exitCode = r.metrics.exitCode; continue; }
  if (r.metrics.crashed === 1) g.crashed = true;
  for (const [k, v] of Object.entries(r.metrics)) (g.metrics[k] ??= []).push(v);
}

const fmtNum = (v, d) => (Math.abs(v) >= 10000 ? Math.round(v).toLocaleString("en-US") : v.toFixed(d));
const cell = (g, metric, d = 1) => {
  const a = g.metrics[metric];
  if (!a?.length) return g.crashed ? "**crash**" : "—";
  if (a.length === 1) return fmtNum(a[0], d);
  return `${fmtNum(median(a), d)} [${fmtNum(Math.min(...a), d)}–${fmtNum(Math.max(...a), d)}] ±${fmtNum(stddev(a), d)}`;
};

// What each experiment's table shows: [metric, header, decimals]
const COLUMNS = {
  1: [["wallMs", "wall ms", 0], ["peakRssMb", "peak RSS MiB", 0], ["cpuMs", "CPU ms", 0]],
  2: [["jobsPerSec", "jobs/s", 0], ["cpuUtil", "CPU use (of 2)", 2], ["peakRssMb", "peak RSS MiB", 0]],
  3: [["wallMs", "total ms", 0], ["overheadP50Ms", "overhead p50 ms", 2], ["overheadP99Ms", "overhead p99 ms", 2], ["peakRssMb", "peak RSS MiB", 0]],
  4: [["nsPerOp", "ns/op", 1], ["correct", "exact result", 0], ["cpuUtil", "CPU use", 2]],
  5: [["detected", "race detected", 0], ["wallMs", "ms", 0], ["runsWithLostUpdates", "runs with lost updates", 0], ["raceReports", "race reports", 0], ["fixPasses", "fix passes", 0]],
  6: [["itemsPerSec", "items/s", 0], ["dropped", "dropped", 0], ["producerBlockedMs", "producer blocked ms", 0], ["correct", "no loss", 0]],
  7: [["stopMs", "stop after cancel ms", 2], ["leaked", "leaked tasks", 0]],
  8: [["detected", "detected", 0], ["detectMs", "time to detect ms", 0], ["leaked", "leaked", 0]],
  9: [["linesPerSec", "lines/s", 0], ["wallMs", "ms", 0], ["cpuUtil", "CPU use", 2], ["peakRssMb", "peak RSS MiB", 0]],
};

function table(exp) {
  const cols = COLUMNS[exp];
  const list = groups[exp] ?? [];
  if (!list.length) return "_no results yet: run `make lab-go` and `make lab-java`_";
  let md = `| Language | Variant | Parameter | ${cols.map((c) => c[1]).join(" | ")} |\n|---|---|---|${cols.map(() => "---:").join("|")}|\n`;
  for (const g of list) {
    const crash = g.crashed && !Object.keys(g.metrics).length ? ` (exit ${g.exitCode})` : "";
    md += `| ${g.lang} | ${g.variant} | ${g.param || "—"} | ${cols.map(([m, , d]) => cell(g, m, d)).join(" | ")}${crash} |\n`;
  }
  const crashes = list.filter((g) => g.notes);
  for (const g of crashes) md += `\n> ${g.lang} ${g.variant} ${g.param}: crashed, exit ${g.exitCode}. ${g.notes.trim().slice(0, 240)}\n`;
  return md + "\nMedian of the measured repetitions, [min–max] ±standard deviation.";
}

// ---------------------------------------------------------------- charts (horizontal bars, median + min-max whisker)

const COLOR = { go: "#2a78d6", java: "#eb6834" };
const CHART_METRIC = { 1: ["wallMs", "wall time (ms), lower is better"], 2: ["jobsPerSec", "jobs per second, higher is better"],
  3: ["overheadP99Ms", "p99 scheduling overhead (ms), lower is better"], 4: ["nsPerOp", "ns per increment, lower is better"],
  6: ["itemsPerSec", "items per second, higher is better"], 7: ["stopMs", "ms from cancel to all stopped, lower is better"],
  9: ["linesPerSec", "lines per second, higher is better"] };
const METRIC_LABEL = { peakRssMb: "peak resident memory (MiB), lower is better" };
const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;");

function chart(exp, metric = CHART_METRIC[exp]?.[0], label = CHART_METRIC[exp]?.[1], file = `lab-exp${exp}.svg`) {
  const list = (groups[exp] ?? []).filter((g) => g.metrics[metric]?.length || g.crashed);
  if (!list.length) return "";
  const W = 760, rowH = 26, left = 250, right = 90, top = 46;
  const H = top + list.length * rowH + 30;
  const vals = list.flatMap((g) => g.metrics[metric] ?? []);
  const raw = Math.max(...vals) || 1;
  const step = (() => { const s = raw / 4; const p = 10 ** Math.floor(Math.log10(s)); return [1, 2, 2.5, 5, 10].map((k) => k * p).find((k) => k >= s); })();
  const max = step * 4; // round axis: 4 ticks of 1, 2, 2.5 or 5 x 10^n
  const x = (v) => left + (v / max) * (W - left - right);
  const out = [`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${W} ${H}" width="${W}" height="${H}" font-family="system-ui, sans-serif" font-size="12" role="img" aria-label="Experiment ${exp}: ${esc(label)}">`,
    `<style>.bg{fill:#fcfcfb}.ink{fill:#0b0b0b}.ink2{fill:#52514e}.grid{stroke:#e6e5e0}.whisk{stroke:#0b0b0b;stroke-opacity:.6}@media (prefers-color-scheme: dark){.bg{fill:#1c1c1b}.ink{fill:#f2f1ed}.ink2{fill:#b5b3ad}.grid{stroke:#3a3936}.whisk{stroke:#f2f1ed}}</style>`,
    `<rect class="bg" width="100%" height="100%"/>`,
    `<text class="ink" x="12" y="20" font-size="14" font-weight="600">Experiment ${exp}</text>`,
    `<text class="ink2" x="12" y="37">${esc(label)} · bar = median, whisker = min–max</text>`];
  for (let t = 0; t <= 4; t++) {
    const v = (max / 4) * t;
    out.push(`<line class="grid" x1="${x(v)}" x2="${x(v)}" y1="${top - 4}" y2="${H - 24}"/><text class="ink2" x="${x(v)}" y="${H - 8}" text-anchor="middle">${fmtNum(v, v < 10 ? 1 : 0)}</text>`);
  }
  list.forEach((g, i) => {
    const y = top + i * rowH;
    out.push(`<text class="ink" x="${left - 8}" y="${y + 16}" text-anchor="end">${esc(`${g.lang} · ${g.variant}${g.param ? " · " + g.param : ""}`)}</text>`);
    const a = g.metrics[metric];
    if (!a?.length) { out.push(`<text class="ink2" x="${left + 4}" y="${y + 16}">crashed (exit ${g.exitCode ?? "?"})</text>`); return; }
    const m = median(a);
    out.push(`<rect x="${left}" y="${y + 4}" width="${Math.max(1, x(m) - left)}" height="${rowH - 9}" rx="3" fill="${COLOR[g.lang]}"><title>${fmtNum(m, 2)}</title></rect>`);
    if (a.length > 1) out.push(`<line x1="${x(Math.min(...a))}" x2="${x(Math.max(...a))}" y1="${y + 12}" y2="${y + 12}" class="whisk"/>`);
    out.push(`<text class="ink" x="${x(Math.max(...a)) + 6}" y="${y + 16}">${fmtNum(m, m < 10 ? 2 : 0)}</text>`);
  });
  out.push(`<rect x="${W - 150}" y="10" width="10" height="10" fill="${COLOR.go}"/><text class="ink" x="${W - 136}" y="19">Go</text><rect x="${W - 100}" y="10" width="10" height="10" fill="${COLOR.java}"/><text class="ink" x="${W - 86}" y="19">Java</text></svg>`);
  writeFileSync(join(img, file), out.join("\n"));
  return `![Experiment ${exp}: ${label}](img/${file})`;
}

// ---------------------------------------------------------------- lines of code

/** Code lines: not blank, not only a comment. */
function loc(text) {
  let inBlock = false;
  return text.split("\n").filter((l) => {
    const t = l.trim();
    if (inBlock) { if (t.includes("*/")) inBlock = false; return false; }
    if (t.startsWith("/*")) { inBlock = !t.includes("*/"); return false; }
    return t && !t.startsWith("//") && !t.startsWith("*") && !t.startsWith("#") && !t.startsWith("--");
  }).length;
}

/** Splits a source file on its "N." section markers and returns {N: code lines}. */
function sections(file, marker) {
  const text = readFileSync(file, "utf8");
  const parts = {};
  let cur = null;
  for (const line of text.split("\n")) {
    const m = line.match(marker);
    if (m) { cur = Number(m[1]); parts[cur] = ""; continue; }
    if (cur !== null) parts[cur] += line + "\n";
  }
  return Object.fromEntries(Object.entries(parts).map(([k, v]) => [k, loc(v)]));
}

const goLoc = sections(join(here, "go", "exps.go"), /^\/\/ ---- (\d+)\./);
const javaLoc = sections(join(here, "java", "src", "main", "java", "lab", "Main.java"), /\/\/ -+ (\d+)\./);
goLoc[5] = loc(readFileSync(join(here, "go", "race", "counter.go"), "utf8")) + loc(readFileSync(join(here, "go", "race", "counter_test.go"), "utf8"));

const locSummary = () => {
  let md = "| # | Go code lines | Java code lines |\n|---|---:|---:|\n";
  for (const n of [1, 2, 3, 4, 5, 6, 7, 8, 9]) md += `| ${n} | ${goLoc[n] ?? "—"} | ${javaLoc[n] ?? "—"} |\n`;
  return md + "\nCode lines of each experiment (blank and comment lines excluded); the harness (timing, JSON) is not counted.";
};

function walk(dir, pred, acc = []) {
  if (!existsSync(dir)) return acc;
  for (const e of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, e.name);
    if (e.isDirectory()) { if (!["target", "node_modules", ".mvn"].includes(e.name)) walk(p, pred, acc); }
    else if (pred(p)) acc.push(p);
  }
  return acc;
}
const sum = (files) => files.reduce((s, f) => s + loc(readFileSync(f, "utf8")), 0);

function exp10() {
  const goDir = join(root, "services", "ratelimiter-go");
  const jDir = join(root, "services", "ratelimiter-java", "src");
  const goMain = walk(goDir, (p) => p.endsWith(".go") && !p.endsWith("_test.go"));
  const goTest = walk(goDir, (p) => p.endsWith("_test.go"));
  const jMain = walk(join(jDir, "main"), (p) => p.endsWith(".java"));
  const jTest = walk(join(jDir, "test"), (p) => p.endsWith(".java"));
  const lua = walk(join(goDir, "pkg", "limiter", "scripts"), (p) => p.endsWith(".lua"));
  return `| | ratelimiter-go | ratelimiter-java |\n|---|---:|---:|\n` +
    `| source files | ${goMain.length} | ${jMain.length} |\n| code lines (main) | ${sum(goMain)} | ${sum(jMain)} |\n` +
    `| test files | ${goTest.length} | ${jTest.length} |\n| code lines (tests) | ${sum(goTest)} | ${sum(jTest)} |\n` +
    `| shared Lua scripts (identical, counted once) | ${sum(lua)} | ${sum(lua)} |\n` +
    `\nCounted by report.mjs (blank and comment-only lines excluded), same rule for both languages.`;
}

// ---------------------------------------------------------------- flame graphs

function flame(name) {
  const dir = join(results, "profiles");
  const goFile = join(dir, `${name}.traces.txt`);
  const jFile = join(dir, `${name}.samples.txt`);
  const kind = name.startsWith("go-") ? "go" : "java";
  const file = kind === "go" ? goFile : jFile;
  if (!existsSync(file)) return `_profile ${name} missing_`;
  const svg = flameFromFile(file, kind, `CPU flame graph: ${name} (width = share of samples)`);
  if (!svg) return `_profile ${name} has no samples_`;
  writeFileSync(join(img, `lab-flame-${name}.svg`), svg);
  return `![flame graph ${name}](img/lab-flame-${name}.svg)`;
}

// ---------------------------------------------------------------- template

const env = () => ["go", "java"].map((l) => {
  const f = join(results, `env-${l}.txt`);
  return existsSync(f) ? "```\n" + readFileSync(f, "utf8").trim() + "\n```" : `_env-${l}.txt missing_`;
}).join("\n\n");

let doc = readFileSync(join(here, "report-template.md"), "utf8");
doc = doc.replace(/\{\{env\}\}/g, env)
  .replace(/\{\{table:(\d+)\}\}/g, (_, n) => table(Number(n)))
  .replace(/\{\{chart:(\d+)(?::(\w+))?\}\}/g, (_, n, metric) => metric
    ? chart(Number(n), metric, METRIC_LABEL[metric] ?? metric, `lab-exp${n}-${metric}.svg`) : chart(Number(n)))
  .replace(/\{\{loc:(\d+)\}\}/g, (_, n) => `Go ${goLoc[n] ?? "—"} code lines, Java ${javaLoc[n] ?? "—"} code lines.`)
  .replace(/\{\{loc-summary\}\}/g, locSummary)
  .replace(/\{\{exp10\}\}/g, exp10)
  .replace(/\{\{flame:([\w.-]+)\}\}/g, (_, n) => flame(n));
writeFileSync(join(root, "docs", "concurrency-comparison.md"), doc);
console.log(`docs/concurrency-comparison.md written from ${rows.length} result lines`);

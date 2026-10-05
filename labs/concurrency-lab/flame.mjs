// Flame graphs without extra tools: parses the text output of `go tool pprof -traces` and of
// `jfr print --events jdk.ExecutionSample`, folds the stacks and draws an SVG (root at the bottom, width = share of samples).
import { readFileSync } from "node:fs";

/** pprof -traces: blocks separated by "-----------+---", first line "  <value><unit>   <leaf frame>", then callers. */
export function foldPprof(text) {
  const folded = new Map();
  for (const block of text.split(/^-+\+-+$/m)) {
    const lines = block.split("\n").filter((l) => l.trim() && !/^(File|Build ID|Type|Time|Duration|Samples):/.test(l.trim()));
    if (!lines.length) continue;
    const m = lines[0].match(/^\s*([\d.]+)(ns|us|µs|ms|s)\s+(.*)$/);
    if (!m) continue;
    const unit = { ns: 1e-6, us: 1e-3, "µs": 1e-3, ms: 1, s: 1000 }[m[2]];
    const frames = [m[3].trim(), ...lines.slice(1).map((l) => l.trim())].filter((f) => f && !f.startsWith("bytes:"));
    const key = frames.reverse().join(";");
    folded.set(key, (folded.get(key) ?? 0) + parseFloat(m[1]) * unit);
  }
  return folded;
}

/** jfr print: each sample has `stackTrace = [ frame line: n ... ]`, top frame first. One sample = weight 1. */
export function foldJfr(text) {
  const folded = new Map();
  for (const m of text.matchAll(/stackTrace = \[\n([\s\S]*?)\n\s*\]/g)) {
    const frames = m[1].split("\n").map((l) => l.trim()).filter((l) => l && l !== "...")
      .map((l) => l.replace(/\s+line: \d+.*$/, "").replace(/\(.*\)$/, ""));
    const key = frames.reverse().join(";");
    folded.set(key, (folded.get(key) ?? 0) + 1);
  }
  return folded;
}

function tree(folded) {
  const root = { name: "all", value: 0, children: new Map() };
  for (const [stack, v] of folded) {
    root.value += v;
    let node = root;
    for (const f of stack.split(";")) {
      if (!node.children.has(f)) node.children.set(f, { name: f, value: 0, children: new Map() });
      node = node.children.get(f);
      node.value += v;
    }
  }
  return root;
}

const esc = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

/** Renders folded stacks as an SVG flame graph. Frames under `minShare` of the total are merged away. */
export function flameSvg(folded, title, { width = 1200, row = 17, minShare = 0.005 } = {}) {
  const root = tree(folded);
  const rects = [];
  let depth = 0;
  const walk = (node, x, d) => {
    depth = Math.max(depth, d);
    rects.push({ node, x, d });
    let cx = x;
    for (const c of [...node.children.values()].sort((a, b) => a.name.localeCompare(b.name))) {
      if (c.value / root.value >= minShare) walk(c, cx, d + 1);
      cx += c.value;
    }
  };
  walk(root, 0, 0);
  const scale = (width - 20) / root.value;
  depth = Math.max(...rects.filter((r) => r.node.value * scale >= 0.5).map((r) => r.d)); // only drawn frames count
  const h = (depth + 1) * row + 40;
  const color = (name) => { // warm palette, stable per frame name; runtime/JVM frames greyer
    let hsh = 0;
    for (const ch of name) hsh = (hsh * 31 + ch.charCodeAt(0)) >>> 0;
    const runtime = /^(runtime\.|java\.lang\.Thread|jdk\.internal|java\.util\.concurrent)/.test(name);
    return runtime ? `hsl(30, 12%, ${70 + (hsh % 12)}%)` : `hsl(${10 + (hsh % 40)}, ${70 + (hsh % 20)}%, ${58 + (hsh % 14)}%)`;
  };
  const out = [`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 ${width} ${h}" width="${width}" height="${h}" font-family="system-ui, sans-serif" font-size="11">`,
    `<rect width="100%" height="100%" fill="#fcfcfb"/>`,
    `<text x="10" y="18" font-size="14" fill="#0b0b0b">${esc(title)}</text>`];
  for (const { node, x, d } of rects) {
    const w = node.value * scale;
    if (w < 0.5) continue;
    const y = h - (d + 1) * row - 4;
    const pct = ((node.value / root.value) * 100).toFixed(1);
    const label = w > 40 ? esc(node.name.length * 6.2 > w - 6 ? node.name.slice(0, Math.max(0, Math.floor((w - 6) / 6.2) - 1)) + "…" : node.name) : "";
    out.push(`<g><title>${esc(node.name)} (${pct}%)</title><rect x="${(10 + x * scale).toFixed(1)}" y="${y}" width="${(w - 0.5).toFixed(1)}" height="${row - 1}" rx="2" fill="${color(node.name)}"/>` +
      (label ? `<text x="${(13 + x * scale).toFixed(1)}" y="${y + row - 5}" fill="#1a1a1a">${label}</text>` : "") + `</g>`);
  }
  out.push("</svg>");
  return out.join("\n");
}

export function flameFromFile(file, kind, title) {
  const text = readFileSync(file, "utf8");
  const folded = kind === "go" ? foldPprof(text) : foldJfr(text);
  return folded.size ? flameSvg(folded, title) : null;
}

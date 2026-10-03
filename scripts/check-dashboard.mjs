// Runs every PromQL expression of the Grafana dashboard against Prometheus and reports which ones return data.
//   node scripts/check-dashboard.mjs [prometheus-url]
// An empty result is not always a bug (an error rate with no errors, a service that is not running), so the output
// is a checklist to read, not a pass/fail gate. Exit code 1 only when Prometheus rejects an expression (syntax error).
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";

const prom = process.argv[2] ?? "http://localhost:9090";
const file = fileURLToPath(new URL("../deploy/compose/grafana/dashboards/flashsale.json", import.meta.url));
const dash = JSON.parse(readFileSync(file, "utf8"));

let bad = 0;
for (const p of dash.panels.filter((p) => p.targets)) {
  for (const t of p.targets) {
    const url = `${prom}/api/v1/query?query=${encodeURIComponent(t.expr)}`;
    const res = await (await fetch(url)).json();
    if (res.status !== "success") {
      bad++;
      console.log(`ERROR  ${p.title}: ${res.error}\n       ${t.expr}`);
      continue;
    }
    const n = res.data.result.length;
    console.log(`${n > 0 ? "data " : "empty"}  ${String(n).padStart(2)} series  ${p.title}  [${t.refId}]`);
  }
}
process.exit(bad ? 1 : 0);

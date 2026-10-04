"""Availability probe for make lab-rolling: GET /readyz on the gateway NodePort of every k3s node, once per second,
until the file /tmp/probe.stop exists. Prints one summary line per node and the longest outage."""
import os
import sys
import time
import urllib.request

nodes = sys.argv[1:] or ["172.30.0.11", "172.30.0.12"]
stats = {n: {"ok": 0, "failed": 0, "streak": 0, "worst": 0} for n in nodes}
while not os.path.exists("/tmp/probe.stop"):
    for n in nodes:
        try:
            ok = urllib.request.urlopen(f"http://{n}:30088/readyz", timeout=2).status == 200
        except Exception:
            ok = False
        s = stats[n]
        if ok:
            s["ok"] += 1
            s["streak"] = 0
        else:
            s["failed"] += 1
            s["streak"] += 1
            s["worst"] = max(s["worst"], s["streak"])
    time.sleep(1)
for n, s in stats.items():
    print(f"{n}: {s['ok']} ok, {s['failed']} failed, longest outage {s['worst']} consecutive probes")

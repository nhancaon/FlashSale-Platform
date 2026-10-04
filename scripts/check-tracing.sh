#!/usr/bin/env bash
# Sends one order through the gateway and checks that Jaeger holds ONE trace spanning gateway -> order -> inventory
# (and Oracle). Needs: make up, make up-apps. Jaeger UI: http://localhost:16686
set -euo pipefail
cd "$(dirname "$0")/.."
set -a; . ./.env; set +a

GW=http://localhost:8088
JAEGER=http://localhost:16686
fail() { echo "FAIL: $*" >&2; exit 1; }
json() { node -e 'let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{const o=JSON.parse(s);const v=process.argv[1].split(".").reduce((a,k)=>a?.[k],o);console.log(v??"")})' "$1"; }

user="trace-$RANDOM$RANDOM"
token=$(curl -s -X POST "$GW/auth/login" -H 'Content-Type: application/json' -d "{\"username\":\"$user\",\"password\":\"$DEMO_PASSWORD\"}" | json accessToken)
[ -n "$token" ] || fail "login failed"

trace_id=$(printf '%032x' $((RANDOM * RANDOM * RANDOM)) | cut -c1-32)
span_id=$(printf '%016x' $((RANDOM * RANDOM)) | cut -c1-16)
code=$(curl -s -o /dev/null -w '%{http_code}' -X POST "$GW/api/orders" -H "Authorization: Bearer $token" \
  -H "Idempotency-Key: $user" -H 'Content-Type: application/json' \
  -H "traceparent: 00-$trace_id-$span_id-01" -d '{"items":[{"sku":"SKU-HEADSET","qty":1}]}')
[ "$code" = 201 ] || fail "order was not created (HTTP $code)"
echo "order created, trace id $trace_id (sent by the client in the traceparent header)"

for _ in $(seq 1 30); do
  body=$(curl -s "$JAEGER/api/traces/$trace_id" || true)
  services=$(echo "$body" | node -e '
    let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{try{
      const t=JSON.parse(s).data?.[0]; if(!t){console.log("");return}
      const names=new Set(Object.values(t.processes).map(p=>p.serviceName)); console.log([...names].sort().join(","))}catch(e){console.log("")}})')
  case ",$services," in *,gateway,*,order,*) break ;; esac
  sleep 1
done
echo "services in the trace: ${services:-none}"
for want in gateway order; do
  case ",$services," in *,$want,*) ;; *) fail "service $want is missing from the trace" ;; esac
done
case ",$services," in *,inventory-go,*|*,inventory-java,*) ;; *) fail "no inventory service in the trace" ;; esac

echo "$body" | node -e '
  let s="";process.stdin.on("data",d=>s+=d).on("end",()=>{
    const t=JSON.parse(s).data[0]; const proc=t.processes;
    const byStart=[...t.spans].sort((a,b)=>a.startTime-b.startTime);
    console.log("spans (start order, ms offset, duration ms):");
    for(const sp of byStart.slice(0,14)) console.log("  "+String(Math.round((sp.startTime-byStart[0].startTime)/1000)).padStart(5)+" "+String(Math.round(sp.duration/1000)).padStart(5)+"  "+proc[sp.processID].serviceName.padEnd(15)+sp.operationName);
    console.log("  ... "+t.spans.length+" spans in total");
  })'
echo "TRACING OK"

#!/usr/bin/env bash
# Worker-pool soak: fire N concurrent runs as DISTINCT persons against a running
# gateway and check they run in PARALLEL (total wall-time ≈ slowest request, not
# the sum). Distinct platform_user_id is required — the per-person guard
# serializes runs for the same person on purpose.
#
# Usage:
#   1) start a gateway with the pool on:   SELFMIND_WORKERS=4 selfmind gateway run
#      (or detached:                       SELFMIND_WORKERS=4 selfmind gateway start)
#   2) in another shell:                   N=4 ./scripts/soak-workers.sh
#   3) for the baseline, repeat with the gateway started at SELFMIND_WORKERS=1
#      and compare total wall-time.
#
# Env: ADDR (default 127.0.0.1:8765), N (default 4), PROMPT, TIMEOUT (default 180s).
set -u
ADDR="${ADDR:-127.0.0.1:8765}"
N="${N:-4}"
PROMPT="${PROMPT:-用一句话解释什么是 goroutine}"
TIMEOUT="${TIMEOUT:-180}"

echo "Health:"; curl -sS -m 5 "http://$ADDR/health" || { echo "gateway not reachable at $ADDR"; exit 1; }
echo; echo "Firing $N concurrent requests (distinct persons) ..."

start=$(date +%s.%N)
pids=()
for i in $(seq 1 "$N"); do
  curl -sS -m "$TIMEOUT" -X POST "http://$ADDR/v1/message" \
    -H 'Content-Type: application/json' \
    -d "{\"platform\":\"cli\",\"platform_user_id\":\"soak-$i\",\"channel\":\"soak-$i\",\"content\":\"$PROMPT\"}" \
    -o "/tmp/soak-$i.json" \
    -w "  req $i: http=%{http_code} time=%{time_total}s" >/tmp/soak-$i.timing 2>&1 &
  pids+=($!)
done
for p in "${pids[@]}"; do wait "$p"; done
end=$(date +%s.%N)

cat /tmp/soak-*.timing; echo
total=$(echo "$end - $start" | bc)
echo "TOTAL wall time: ${total}s for $N concurrent requests."
echo
echo "Interpretation:"
echo "  • WORKERS=4 → total ≈ the SLOWEST single request (they ran in parallel)."
echo "  • WORKERS=1 → total ≈ the SUM of all requests (serialized)."
echo "  • Any non-200 http codes / errors in /tmp/soak-*.json indicate a problem."

#!/usr/bin/env bash
# The M0 exit check: the stack comes up, answers through Caddy on one origin, and the
# request it served produced a trace. Run from the repo root: bash deploy/smoke.sh
set -euo pipefail

COMPOSE="docker compose -f deploy/compose.yaml --env-file .env"
BASE="${PLIMSOLL_SMOKE_BASE:-http://localhost:8080}"

echo "--- healthz through caddy ---"
body=$(curl -fsS "$BASE/api/healthz")
echo "$body"
grep -q '"status":"ok"' <<<"$body"
grep -q '"database":"ok"' <<<"$body"

echo "--- the api is reachable only through caddy ---"
# K27 is one origin, not two. If the api port were published as well, a browser could
# reach it directly and the same-origin cookie story would be a claim rather than a fact.
if curl -fsS --max-time 2 http://localhost:8000/healthz >/dev/null 2>&1; then
	echo "FAIL: the api answered on :8000; it must not publish a host port"
	exit 1
fi

echo "--- an authenticated route still refuses without a session ---"
code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/api/me")
[ "$code" = "401" ] || { echo "FAIL: /api/me returned $code, expected 401"; exit 1; }

echo "--- trace reached jaeger ---"
# The exporter batches; give it one interval before asking.
sleep 3
services=$(curl -fsS http://localhost:16686/api/services)
grep -q 'plimsoll-api' <<<"$services" || {
	echo "FAIL: no plimsoll-api service in jaeger: $services"
	exit 1
}

echo "--- worker is running ---"
$COMPOSE ps worker | grep -Eq 'running|Up'

echo "SMOKE: OK"

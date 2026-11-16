#!/usr/bin/env bash
# Checks a running server from outside, the way the app uses it:
# health check, device sign-in with the server key, two RPCs.
#   smoke-test.sh <base-url> <server-key>     e.g. https://13.234.5.6 tango-abc...
set -euo pipefail
base=${1:?base url}; key=${2:?server key}
insecure=""; [ "${SMOKE_INSECURE:-0}" = 1 ] && insecure="-k"

for i in $(seq 1 30); do
  curl -fsS $insecure --max-time 5 "$base/healthcheck" >/dev/null 2>&1 && break
  [ "$i" = 30 ] && { echo "health check failed: $base/healthcheck" >&2; exit 1; }
  sleep 3
done
echo "health check OK"

token=$(curl -fsS $insecure --max-time 10 -u "${key}:" -H 'Content-Type: application/json' \
  -d '{"id":"tango-smoke-test-device-0001"}' \
  "$base/v2/account/authenticate/device?create=true&username=smoketest" | jq -r .token)
[ -n "$token" ] && [ "$token" != null ] || { echo "sign-in failed" >&2; exit 1; }
echo "device sign-in OK"

for rpc in tango.app.config tango.profile.get; do
  out=$(curl -fsS $insecure --max-time 10 -H "Authorization: Bearer $token" -H 'Content-Type: application/json' \
    -d '{}' "$base/v2/rpc/$rpc?unwrap=true")
  echo "$out" | jq -e '.success == true' >/dev/null || { echo "$rpc failed: $out" >&2; exit 1; }
  echo "$rpc OK"
done

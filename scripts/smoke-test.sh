#!/usr/bin/env bash
# Checks the deployed server from the outside the way the app uses it:
# HTTPS health check, device sign-in with the server key, one RPC.
set -euo pipefail
: "${HOSTNAME_PUBLIC:?}" "${PARAM_PATH:?}"

server_key=$(aws ssm get-parameter --name "${PARAM_PATH}/server_key" --with-decryption --query Parameter.Value --output text)

scheme=https
ok=""
for _ in $(seq 1 30); do
  if curl -fsS --max-time 5 "https://${HOSTNAME_PUBLIC}/healthcheck" >/dev/null 2>&1; then ok=1; break; fi
  sleep 5
done
if [ -z "$ok" ]; then
  echo "::warning::HTTPS is not answering (certificate not issued yet?). Trying HTTP."
  scheme=http
  curl -fsS --max-time 5 "http://${HOSTNAME_PUBLIC}/healthcheck" >/dev/null
fi
base="${scheme}://${HOSTNAME_PUBLIC}"
echo "Health check OK: ${base}/healthcheck"

token=$(curl -fsS --max-time 10 -u "${server_key}:" -H 'Content-Type: application/json' \
  -d '{"id":"tango-smoke-test-device-0001"}' \
  "${base}/v2/account/authenticate/device?create=true&username=smoketest" | jq -r .token)
[ -n "$token" ] && [ "$token" != "null" ] || { echo "::error::sign-in failed"; exit 1; }
echo "Device sign-in OK"

config=$(curl -fsS --max-time 10 -H "Authorization: Bearer ${token}" -H 'Content-Type: application/json' \
  -d '{}' "${base}/v2/rpc/tango.app.config?unwrap=true")
echo "$config" | jq -e '.success == true' >/dev/null || { echo "::error::RPC failed: $config"; exit 1; }
profile=$(curl -fsS --max-time 10 -H "Authorization: Bearer ${token}" -d '{}' "${base}/v2/rpc/tango.profile.get?unwrap=true")
echo "$profile" | jq -e '.success == true' >/dev/null || { echo "::error::profile RPC failed: $profile"; exit 1; }
echo "RPCs OK"

ws="wss"
[ "$scheme" = "http" ] && ws="ws"
{
  echo "## Tango Cards nonprod server is live"
  echo
  echo "| Setting (Unity / Nakama client) | Value |"
  echo "|---|---|"
  echo "| Scheme | \`${scheme}\` |"
  echo "| Host | \`${HOSTNAME_PUBLIC}\` |"
  echo "| Port | \`$([ "$scheme" = https ] && echo 443 || echo 80)\` |"
  echo "| Server key | \`${server_key}\` |"
  echo "| Health check | ${base}/healthcheck |"
  echo "| Realtime socket | \`${ws}://${HOSTNAME_PUBLIC}/ws\` |"
  echo
  echo "Server image: \`${IMAGE:-see deploy job}\`"
} >> "${GITHUB_STEP_SUMMARY:-/dev/stdout}"

#!/usr/bin/env bash
# CI stand-in for the host's certbot (nonprod has Certbot in webroot mode).
# "certonly" proves control the way Let's Encrypt's HTTP-01 check does - a
# file in the webroot fetched over http://<domain>/ - then issues a
# self-signed certificate and runs the deploy hook. "certificates" prints the
# path line deploy.sh reads.
set -euo pipefail
cmd=${1:?}; shift
name="" domain="" webroot="" hook=""
while [ $# -gt 0 ]; do
  case $1 in
    -w|--webroot-path) webroot=$2; shift ;;
    -d) domain=$2; shift ;;
    --cert-name) name=$2; shift ;;
    --deploy-hook) hook=$2; shift ;;
  esac
  shift
done
case $cmd in
  certonly)
    tok="ci-$RANDOM$RANDOM"
    mkdir -p "$webroot/.well-known/acme-challenge"
    echo "$tok" > "$webroot/.well-known/acme-challenge/$tok"
    got=$(curl -fsS --max-time 5 "http://$domain/.well-known/acme-challenge/$tok" || true)
    rm -f "$webroot/.well-known/acme-challenge/$tok"
    [ "$got" = "$tok" ] || { echo "fake certbot: challenge failed for $domain" >&2; exit 1; }
    live=/etc/letsencrypt/live/$name
    if [ ! -f "$live/fullchain.pem" ] || [ "${FORCE_RENEWAL:-0}" = 1 ]; then
      mkdir -p "$live"
      openssl req -x509 -newkey rsa:2048 -nodes -days 30 -subj "/CN=$domain" \
        -addext "subjectAltName=DNS:$domain" -keyout "$live/privkey.pem" -out "$live/fullchain.pem" 2>/dev/null
      [ -z "$hook" ] || RENEWED_LINEAGE=$live "$hook"
    fi
    ;;
  certificates)
    echo "  Certificate Name: $name"
    echo "    Certificate Path: /etc/letsencrypt/live/$name/fullchain.pem"
    ;;
esac

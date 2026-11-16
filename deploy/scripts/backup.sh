#!/usr/bin/env bash
# Nightly database backup to /opt/tango/backups (keeps the newest 7).
set -euo pipefail
dir=${TANGO_DIR:-/opt/tango}
keep=${TANGO_BACKUPS_KEEP:-7}
mkdir -p "$dir/backups"
out="$dir/backups/tango-$(date +%Y-%m-%dT%H%M%S).sql.gz"
docker compose --project-directory "$dir" exec -T postgres pg_dump -U tango -d tango --no-owner | gzip > "$out.tmp"
mv "$out.tmp" "$out"
ls -1t "$dir"/backups/tango-*.sql.gz | tail -n +$((keep + 1)) | xargs -r rm -f
echo "backup written: $out"

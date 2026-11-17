#!/usr/bin/env bash
# Deploys (or updates) the Tango Cards server on this machine with Docker
# Compose. Safe to run again and again: secrets are generated once, the
# database is kept, and the two game nodes restart one at a time so no live
# match is interrupted.
#
# It lives side by side with other Docker Compose stacks on the server: its
# own project name, network and volume; it only publishes ports that are
# free; it never prunes other projects' images or containers.
#
# The server binary must already be at $TANGO_DIR/releases/$TANGO_RELEASE/tango-server
# (the Ansible playbook copies it there over SSM).
#
# Usage (as root):
#   TANGO_RELEASE=<sha> TANGO_HOST=<elastic-ip or domain> TANGO_DIR=/opt/tango-server scripts/deploy.sh
#
# Settings (environment):
#   TANGO_RELEASE    release folder name under $TANGO_DIR/releases (required)
#   TANGO_HOST       public IP or domain clients use (required)
#   TANGO_DIR        deploy folder on this server (default /opt/tango-server)
#   TANGO_ENV        nonprod | production (default nonprod)
#   TANGO_TLS        auto | off (auto = free Let's Encrypt certificate, see below)
#   TANGO_PUBLIC_IP  the server's public IP; used in the address while there is no certificate
#   TANGO_ACME_WEBROOT  folder that whatever owns port 80 already serves at
#                    /.well-known/acme-challenge/ (on nonprod: /opt/ludo-acme)
#   TANGO_HTTP_PORT  force the public HTTP port (first deploy only)
#   TANGO_KEEP       releases to keep (default 5)
#   ALLOW_CHEATS     true | false (dev shortcut RPC; always off in production)
#   TANGO_GOOGLE_CLIENT_IDS  comma-separated OAuth client IDs whose Google ID
#                    tokens are accepted for sign-in (the game's Web client ID)
#
# HTTPS (TANGO_TLS=auto, TANGO_HOST is a domain or IP):
#   - ports 80/443 free: Tango's own nginx + certbot container (HTTP-01 on 80).
#   - ports 80/443 owned by another stack (nonprod): the host's certbot issues
#     the certificate through TANGO_ACME_WEBROOT, the folder the other stack's
#     nginx already serves for Let's Encrypt, so its configuration is not
#     touched. Tango serves HTTPS on its own port (HTTPS_PORT in .env) and the
#     host's certbot.timer renews it; a per-certificate deploy hook copies the
#     new files into $TANGO_DIR/certs and reloads Tango's nginx.
#   No certificate yet (DNS not pointing here, check failed): plain HTTP, the
#   deploy still succeeds and the next run tries again.
set -euo pipefail
[ "$(id -u)" = 0 ] || exec sudo -E bash "$0" "$@"

SRC=$(cd "$(dirname "$0")/.." && pwd)
DIR=${TANGO_DIR:-/opt/tango-server}
ENVNAME=${TANGO_ENV:-nonprod}
TLS=${TANGO_TLS:-auto}
CHEATS=${ALLOW_CHEATS:-true}
: "${TANGO_RELEASE:?TANGO_RELEASE is required}"
: "${TANGO_HOST:?TANGO_HOST is required (public IP or domain)}"
[ "$ENVNAME" = production ] && CHEATS=false

log() { printf '\n==> %s\n' "$*"; }
port_free() { ! (ss -ltnH 2>/dev/null || netstat -ltn 2>/dev/null) | awk '{print $4}' | grep -Eq "[:.]$1\$"; }
compose() { docker compose --project-directory "$DIR" "$@"; }
is_ip() { [[ "$1" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]]; }

log "Docker"
bash "$SRC/scripts/install-docker.sh"

RELEASE_DIR="$DIR/releases/$TANGO_RELEASE"
[ -x "$RELEASE_DIR/tango-server" ] || { echo "ERROR: $RELEASE_DIR/tango-server not found" >&2; exit 1; }

log "Disk space"
free_mb=$(df -Pm /var/lib/docker 2>/dev/null | awk 'NR==2{print $4}')
[ -n "$free_mb" ] || free_mb=$(df -Pm / | awk 'NR==2{print $4}')
echo "${free_mb} MB free"
if [ "$free_mb" -lt 800 ]; then
  echo "ERROR: less than 800 MB free on the Docker disk; free some space first (nothing was changed)." >&2
  exit 1
fi

log "Files in $DIR"
mkdir -p "$DIR"/{config,nginx,certbot/www,certbot/conf,backups,bin}
install -d -m 0700 "$DIR/certs"
if [ -f "$DIR/docker-compose.yml" ] && ! grep -q '^name: tango-backend' "$DIR/docker-compose.yml"; then
  bak="$DIR/docker-compose.yml.bak-$(date +%Y%m%d%H%M%S)"
  echo "An existing docker-compose.yml in $DIR is not the Tango one; saved it as $bak"
  mv "$DIR/docker-compose.yml" "$bak"
fi
install -m 0644 "$SRC/docker-compose.yml" "$DIR/docker-compose.yml"
install -m 0755 "$SRC/scripts/backup.sh" "$DIR/bin/backup.sh"

# Copies a certificate lineage (fullchain.pem + privkey.pem) into certs/ for
# Tango's nginx and reloads it. Used as certbot's deploy hook (RENEWED_LINEAGE)
# and after the first issue.
cat > "$DIR/bin/sync-cert.sh" <<EOF
#!/bin/sh
# Written by deploy.sh.
set -eu
src=\${RENEWED_LINEAGE:-\${1:?certificate folder}}
dst=$DIR/certs
umask 077
cp -L "\$src/fullchain.pem" "\$dst/fullchain.pem.new"
cp -L "\$src/privkey.pem" "\$dst/privkey.pem.new"
mv "\$dst/fullchain.pem.new" "\$dst/fullchain.pem"
mv "\$dst/privkey.pem.new" "\$dst/privkey.pem"
basename "\$src" > "\$dst/name"
if [ -n "\$(docker compose --project-directory $DIR ps -q nginx 2>/dev/null)" ]; then
  docker compose --project-directory $DIR exec -T nginx nginx -s reload || true
fi
EOF
chmod 0755 "$DIR/bin/sync-cert.sh"

# Renewal for the certbot-container mode (no-op otherwise).
cat > "$DIR/bin/renew-cert.sh" <<EOF
#!/bin/sh
# Written by deploy.sh.
set -eu
[ -d $DIR/certbot/conf/live/tango ] || exit 0
docker compose --project-directory $DIR run --rm certbot renew --webroot -w /var/www/certbot --quiet
$DIR/bin/sync-cert.sh $DIR/certbot/conf/live/tango
EOF
chmod 0755 "$DIR/bin/renew-cert.sh"

# ------------------------------------------------------------ secrets (.env)
# Generated on the first deploy and kept on this server only.
envfile="$DIR/.env"
if [ ! -f "$envfile" ]; then
  log "Generating secrets (first deploy)"
  rnd() { head -c 48 /dev/urandom | od -An -tx1 | tr -d ' \n' | head -c "$1"; }
  umask 077
  cat > "$envfile" <<EOF
# Tango server settings for docker compose. Generated $(date -u +%FT%TZ). Keep private.
POSTGRES_PASSWORD=$(rnd 40)
SESSION_KEY=$(rnd 48)
REFRESH_KEY=$(rnd 48)
HTTP_KEY=$(rnd 40)
CLUSTER_SECRET=$(rnd 48)
SERVER_KEY=tango-$(rnd 24)
TANGO_RELEASE_DIR=
TANGO_HOST=
EOF
  umask 022
fi
chmod 600 "$envfile"
set_env() {
  if grep -q "^$1=" "$envfile"; then sed -i "s|^$1=.*|$1=$2|" "$envfile"; else echo "$1=$2" >> "$envfile"; fi
}
# shellcheck disable=SC2153 # TANGO_RELEASE comes from the caller
set_env TANGO_RELEASE_DIR "$RELEASE_DIR"
sed -i '/^TANGO_IMAGE=/d' "$envfile"
set_env TANGO_HOST "$TANGO_HOST"

# Public ports, decided once on the first deploy and then kept: 80/443 when
# they are free (HTTPS with Let's Encrypt), otherwise 8350 (plain HTTP) so
# whatever already uses 80/443 on this server keeps working.
if ! grep -q '^HTTP_PORT=' "$envfile"; then
  if [ -n "${TANGO_HTTP_PORT:-}" ]; then
    hp=$TANGO_HTTP_PORT; sp=${TANGO_HTTPS_PORT:-7443}
  elif port_free 80 && port_free 443; then
    hp=80; sp=443
  else
    # 80/443 belong to another stack: first free fallback port (7350 is
    # avoided because Nakama-based servers use it).
    hp=""; sp=""
    for p in 8350 8450 9350 9450; do port_free "$p" && { hp=$p; break; }; done
    for p in 8443 9443 7443 10443; do port_free "$p" && { sp=$p; break; }; done
    [ -n "$hp" ] && [ -n "$sp" ] || { echo "ERROR: no free port found for Tango; set TANGO_HTTP_PORT" >&2; exit 1; }
    echo "Ports 80/443 are used by something else on this server; Tango will listen on port $hp (HTTP)."
  fi
  set_env HTTP_PORT "$hp"
  set_env HTTPS_PORT "$sp"
fi
set -a
# shellcheck source=/dev/null
. "$envfile"
set +a

# ------------------------------------------------------------ node config
log "Server config"
node_cfg="$DIR/config/node.yml"
tmp=$(mktemp)
cat > "$tmp" <<EOF
# tango-server node config ($ENVNAME). Written by deploy.sh.
logger: { level: info, format: json }
database:
  address: ["postgres://tango:${POSTGRES_PASSWORD}@postgres:5432/tango?sslmode=disable"]
  max_open_conns: 40
session:
  encryption_key: "${SESSION_KEY}"
  refresh_encryption_key: "${REFRESH_KEY}"
  token_expiry_sec: 7200
  refresh_token_expiry_sec: 2592000
socket:
  server_key: "${SERVER_KEY}"
  address: 0.0.0.0
  port: 7350
  trusted_proxy_header: X-Forwarded-For
security:
  trusted_proxies: ["10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"]
runtime:
  http_key: "${HTTP_KEY}"
  env:
    - "DEPLOYMENT_ENVIRONMENT=${ENVNAME}"
    - "TANGO_ALLOW_CHEATS=${CHEATS}"
    - "GOOGLE_IAP_ENABLED=${GOOGLE_IAP_ENABLED:-false}"
    - "TANGO_ANDROID_PACKAGE=${TANGO_ANDROID_PACKAGE:-}"
cluster:
  secret: "${CLUSTER_SECRET}"
  roles: [gateway, game]
  bus_port: 7352
  admin_port: 7351
  admin_address: 0.0.0.0
  drain_timeout_ms: 30000
  presence_grace_ms: 10000
metrics: { prometheus_port: 9100 }
EOF
if [ -n "${TANGO_GOOGLE_CLIENT_IDS:-}" ]; then
  # Google sign-in: only ID tokens issued to the game's own OAuth client(s).
  { echo "social:"; echo "  google:"; echo "    client_ids:"
    echo "$TANGO_GOOGLE_CLIENT_IDS" | tr ',' '\n' | sed 's/^ *//;s/ *$//' | grep . | sed 's/.*/      - "&"/'; } >> "$tmp"
fi
cfg_changed=0
if ! cmp -s "$tmp" "$node_cfg" 2>/dev/null; then cfg_changed=1; fi
install -m 0640 -o root -g 65532 "$tmp" "$node_cfg"   # readable by the container user only
rm -f "$tmp"

# ------------------------------------------------------------ nginx config
nginx_locations() {
  cat <<'EOF'
    location ~ ^/(ws|api/v2/socket)$ {
      proxy_pass http://tango;
      proxy_http_version 1.1;
      proxy_set_header Upgrade $http_upgrade;
      proxy_set_header Connection $connection_upgrade;
      proxy_set_header Host $host;
      proxy_set_header X-Forwarded-For $remote_addr;
      proxy_read_timeout 120s;
      proxy_send_timeout 120s;
      proxy_next_upstream error timeout http_502 http_503;
      limit_conn perip 50;
    }
    location ~ ^/(v2/account/authenticate|api/v2/auth) {
      limit_req zone=auth burst=30 nodelay;
      proxy_pass http://tango;
      proxy_http_version 1.1;
      proxy_set_header Connection "";
      proxy_set_header Host $host;
      proxy_set_header X-Forwarded-For $remote_addr;
      proxy_next_upstream error timeout http_502 http_503;
    }
    location / {
      proxy_pass http://tango;
      proxy_http_version 1.1;
      proxy_set_header Connection "";
      proxy_set_header Host $host;
      proxy_set_header X-Forwarded-For $remote_addr;
      proxy_next_upstream error timeout http_502 http_503;
      proxy_read_timeout 30s;
    }
EOF
}
write_nginx() { # $1 = tls (1/0)
  local tls=$1 out="$DIR/nginx/nginx.conf.new"
  {
    cat <<'EOF'
# Written by deploy.sh.
worker_processes auto;
worker_rlimit_nofile 65536;
events { worker_connections 8192; }
http {
  server_tokens off;
  resolver 127.0.0.11 valid=10s ipv6=off;
  log_format main '$remote_addr [$time_local] "$request" $status $body_bytes_sent $request_time';
  access_log /var/log/nginx/access.log main;
  keepalive_timeout 65;
  client_max_body_size 1m;
  map $http_upgrade $connection_upgrade { default upgrade; '' close; }
  limit_req_zone $binary_remote_addr zone=auth:10m rate=60r/m;
  limit_conn_zone $binary_remote_addr zone=perip:10m;
  upstream tango {
    zone tango 64k;
    least_conn;
    server tango-node1:7350 resolve max_fails=1 fail_timeout=3s;
    server tango-node2:7350 resolve max_fails=1 fail_timeout=3s;
    keepalive 32;
  }
  server {
    listen 80;
    listen [::]:80;
    server_name _;
    location /.well-known/acme-challenge/ { root /var/www/certbot; }
EOF
    if [ "$tls" = 1 ] && [ "$HTTP_PORT" = 80 ]; then
      echo '    location / { return 301 https://$host$request_uri; }'
    else
      # Plain HTTP keeps working on a non-standard port (existing clients).
      nginx_locations
    fi
    echo '  }'
    if [ "$tls" = 1 ]; then
      cat <<'EOF'
  server {
    listen 443 ssl;
    listen [::]:443 ssl;
    http2 on;
    server_name _;
    ssl_certificate     /etc/tango-certs/fullchain.pem;
    ssl_certificate_key /etc/tango-certs/privkey.pem;
    ssl_protocols TLSv1.2 TLSv1.3;
    ssl_session_cache shared:SSL:10m;
EOF
      nginx_locations
      echo '  }'
    fi
    echo '}'
  } > "$out"
  if cmp -s "$out" "$DIR/nginx/nginx.conf" 2>/dev/null; then rm -f "$out"; return 1; fi
  # Rewrite in place: nginx.conf is bind-mounted as a single file, so a new
  # inode (mv) would not be seen by the running container.
  cat "$out" > "$DIR/nginx/nginx.conf"
  rm -f "$out"
  return 0
}

# Certificate mode: "container" when port 80 is Tango's, "host" when another
# stack owns 80 but serves TANGO_ACME_WEBROOT and this host has certbot.
cert_mode=none
if [ "$TLS" = auto ]; then
  if [ "$HTTP_PORT" = 80 ]; then
    cert_mode=container
  elif ! is_ip "$TANGO_HOST" && [ -n "${TANGO_ACME_WEBROOT:-}" ] && [ -d "$TANGO_ACME_WEBROOT" ] &&
       command -v certbot >/dev/null 2>&1; then
    cert_mode=host
  fi
fi
# Older deploys kept the container certificate only under certbot/conf.
if [ ! -f "$DIR/certs/fullchain.pem" ] && [ -f "$DIR/certbot/conf/live/tango/fullchain.pem" ]; then
  "$DIR/bin/sync-cert.sh" "$DIR/certbot/conf/live/tango"
fi
cert_name=tango; [ "$cert_mode" = host ] && cert_name=$TANGO_HOST
cert="$DIR/certs/fullchain.pem"
have_cert=0
if [ "$cert_mode" != none ] && [ -f "$cert" ] && [ "$(cat "$DIR/certs/name" 2>/dev/null)" = "$cert_name" ]; then
  have_cert=1
fi
nginx_changed=0
if write_nginx "$have_cert"; then nginx_changed=1; fi

# ------------------------------------------------------------ ports free?
nginx_running=$(compose ps -q nginx 2>/dev/null || true)
check_port() {
  if ! port_free "$1"; then
    echo "ERROR: port $1 is already used by another program on this server:" >&2
    (ss -ltnp 2>/dev/null | grep -E "[:.]$1 " || true) >&2
    echo "Set TANGO_HTTP_PORT to a free port (or free this one) and deploy again." >&2
    exit 1
  fi
}
if [ -z "$nginx_running" ]; then
  check_port "$HTTP_PORT"; check_port "$HTTPS_PORT"
fi
for n in 1 2; do
  [ -n "$(compose ps -q "tango-node$n" 2>/dev/null)" ] || check_port "1736$n"
done

# ------------------------------------------------------------ images
log "Pulling images"
pull_list="postgres nginx tango-node1"
for i in 1 2 3; do
  # shellcheck disable=SC2086
  compose pull --quiet $pull_list && break
  [ $i = 3 ] && exit 1
  sleep 10
done
if [ "$cert_mode" = container ]; then
  compose --profile tools pull --quiet certbot
fi

# ------------------------------------------------------------ database
log "Database"
compose up -d --wait postgres
compose run --rm migrate

# ------------------------------------------------------------ game nodes
wait_node() { # $1 admin port
  for _ in $(seq 1 120); do
    if curl -fsS "http://127.0.0.1:$1/admin/health" 2>/dev/null | grep -q '"draining":false'; then return 0; fi
    sleep 1
  done
  echo "node on admin port $1 did not become healthy" >&2
  compose logs --tail 50 >&2 || true
  return 1
}
for node in node1:17361 node2:17362; do
  name=${node%%:*}; port=${node##*:}
  log "Game $name"
  flags=""
  if [ "$cfg_changed" = 1 ] && [ -n "$(compose ps -q "tango-$name" 2>/dev/null)" ]; then flags="--force-recreate"; fi
  # Replacing a running node: Compose sends SIGTERM, the node drains (its
  # matches move to the other node), then the new container starts.
  compose up -d --no-deps $flags "tango-$name"
  wait_node "$port"
done

# ------------------------------------------------------------ nginx + TLS
log "Nginx"
compose up -d nginx
if [ -n "$nginx_running" ] && [ "$nginx_changed" = 1 ]; then
  compose exec -T nginx nginx -s reload
fi

if [ "$cert_mode" = container ] && [ "$have_cert" = 0 ]; then
  log "TLS certificate for $TANGO_HOST (Let's Encrypt)"
  if is_ip "$TANGO_HOST"; then
    ident="--ip-address $TANGO_HOST --preferred-profile shortlived"
  else
    ident="-d $TANGO_HOST"
  fi
  if compose run --rm certbot certonly --webroot -w /var/www/certbot --cert-name tango $ident \
       --register-unsafely-without-email --agree-tos --non-interactive --keep-until-expiring; then
    if is_ip "$TANGO_HOST" && [ -f "$DIR/certbot/conf/renewal/tango.conf" ]; then
      # 6-day IP certificates: renew with two days left, not on every timer run.
      grep -q '^renew_before_expiry' "$DIR/certbot/conf/renewal/tango.conf" ||
        sed -i '/^version/a renew_before_expiry = 2 days' "$DIR/certbot/conf/renewal/tango.conf"
    fi
    "$DIR/bin/sync-cert.sh" "$DIR/certbot/conf/live/tango"
    write_nginx 1 || true
    compose exec -T nginx nginx -s reload
    have_cert=1
  else
    echo "WARNING: certificate not issued; serving plain HTTP for now. Check that ports 80 and 443 are open in the EC2 security group." >&2
  fi
fi

if [ "$cert_mode" = host ]; then
  log "TLS certificate for $TANGO_HOST (host certbot, webroot $TANGO_ACME_WEBROOT)"
  host_cert_ok=0
  dns_ip=$(getent ahostsv4 "$TANGO_HOST" 2>/dev/null | awk 'NR==1{print $1}' || true)
  probe="tango-probe-$(head -c 8 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  challenge_dir="$TANGO_ACME_WEBROOT/.well-known/acme-challenge"
  if [ -z "$dns_ip" ]; then
    echo "WARNING: $TANGO_HOST has no DNS A record yet; serving plain HTTP for now. Add the A record (value: ${TANGO_PUBLIC_IP:-the Elastic IP of this server}) and deploy again." >&2
  elif [ -n "${TANGO_PUBLIC_IP:-}" ] && [ "$dns_ip" != "$TANGO_PUBLIC_IP" ]; then
    echo "WARNING: $TANGO_HOST points to $dns_ip, not this server ($TANGO_PUBLIC_IP); serving plain HTTP for now." >&2
  else
    mkdir -p "$challenge_dir"
    echo "$probe" > "$challenge_dir/$probe"; chmod 0644 "$challenge_dir/$probe"
    # Ask whatever answers port 80 for this name, the way Let's Encrypt
    # will. The other stack may listen on one address only (on nonprod the
    # shared nginx binds the private IP), so try loopback, every local IPv4
    # and finally the public name itself.
    got="" tried=""
    url="http://$TANGO_HOST/.well-known/acme-challenge/$probe"
    for addr in 127.0.0.1 $(hostname -I 2>/dev/null) public; do
      case $addr in *:*) continue ;; esac
      if [ "$addr" = public ]; then
        res=$(curl -sS --max-time 10 -o "$challenge_dir/.tango-got" -w '%{http_code}' "$url" 2>/dev/null || true)
      else
        res=$(curl -sS --max-time 5 -o "$challenge_dir/.tango-got" -w '%{http_code}' \
                --resolve "$TANGO_HOST:80:$addr" "$url" 2>/dev/null || true)
      fi
      tried="$tried $addr=${res:-none}"
      got=$(cat "$challenge_dir/.tango-got" 2>/dev/null || true)
      rm -f "$challenge_dir/.tango-got"
      [ "$res" = 200 ] && [ "$got" = "$probe" ] && break
      got=""
    done
    rm -f "$challenge_dir/$probe"
    if [ "$got" != "$probe" ]; then
      echo "WARNING: the web server on port 80 does not serve $TANGO_ACME_WEBROOT for $TANGO_HOST (HTTP status per address:$tried); serving plain HTTP for now." >&2
    elif certbot certonly --webroot -w "$TANGO_ACME_WEBROOT" -d "$TANGO_HOST" --cert-name "$TANGO_HOST" \
           --deploy-hook "$DIR/bin/sync-cert.sh" --keep-until-expiring \
           --non-interactive --agree-tos --register-unsafely-without-email; then
      live=$(certbot certificates --cert-name "$TANGO_HOST" 2>/dev/null | awk '/Certificate Path:/{print $3; exit}')
      live=${live:+$(dirname "$live")}
      [ -n "$live" ] || live=/etc/letsencrypt/live/$TANGO_HOST
      "$DIR/bin/sync-cert.sh" "$live"
      host_cert_ok=1
    else
      echo "WARNING: Let's Encrypt did not issue the certificate; serving plain HTTP for now." >&2
    fi
  fi
  if [ "$host_cert_ok" = 1 ]; then
    have_cert=1
  fi
  # Serve what we have (HTTPS when the certificate is there).
  [ -f "$cert" ] && [ "$(cat "$DIR/certs/name" 2>/dev/null)" = "$cert_name" ] || have_cert=0
  if write_nginx "$have_cert"; then compose exec -T nginx nginx -s reload; fi
fi

# ------------------------------------------------------------ timers
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  log "Timers (certificate renewal, nightly backup)"
  cat > /etc/systemd/system/tango-certbot-renew.service <<EOF
[Unit]
Description=Renew the Tango TLS certificate
[Service]
Type=oneshot
ExecStart=$DIR/bin/renew-cert.sh
EOF
  cat > /etc/systemd/system/tango-certbot-renew.timer <<'EOF'
[Unit]
Description=Renew the Tango TLS certificate twice a day
[Timer]
OnCalendar=*-*-* 03,15:17:00
RandomizedDelaySec=1800
Persistent=true
[Install]
WantedBy=timers.target
EOF
  cat > /etc/systemd/system/tango-backup.service <<EOF
[Unit]
Description=Back up the Tango database
[Service]
Type=oneshot
Environment=TANGO_DIR=$DIR
ExecStart=$DIR/bin/backup.sh
EOF
  cat > /etc/systemd/system/tango-backup.timer <<'EOF'
[Unit]
Description=Nightly Tango database backup
[Timer]
OnCalendar=*-*-* 02:30:00
Persistent=true
[Install]
WantedBy=timers.target
EOF
  systemctl daemon-reload
  systemctl enable --now tango-certbot-renew.timer tango-backup.timer >/dev/null
fi

# ------------------------------------------------------------ check
log "Health check"
if [ "$have_cert" = 1 ]; then
  scheme=https; port=$HTTPS_PORT; public=$TANGO_HOST
else
  scheme=http; port=$HTTP_PORT; public=${TANGO_PUBLIC_IP:-$TANGO_HOST}
fi
base="$scheme://$public"
{ [ "$scheme" = https ] && [ "$port" = 443 ]; } || { [ "$scheme" = http ] && [ "$port" = 80 ]; } || base="$base:$port"
ok=0
for _ in $(seq 1 30); do
  if curl -fsS --max-time 5 "$base/healthcheck" >/dev/null 2>&1 ||
     curl -fsS --max-time 5 "http://127.0.0.1:$HTTP_PORT/healthcheck" >/dev/null 2>&1; then ok=1; break; fi
  sleep 2
done
[ "$ok" = 1 ] || { echo "ERROR: health check failed" >&2; compose ps >&2; compose logs --tail 80 >&2; exit 1; }

# Keep the newest releases (the running one is always kept).
keep=${TANGO_KEEP:-5}
{ find "$DIR/releases" -mindepth 1 -maxdepth 1 -type d -printf '%T@ %f\n' | sort -rn | awk '{print $2}' |
    grep -vxF "$TANGO_RELEASE" || true; } | tail -n +"$keep" |
  while read -r old; do rm -rf -- "${DIR:?}/releases/$old"; done

cat > "$DIR/connection.txt" <<EOF
Tango Cards server ($ENVNAME)
  Base       : $base
  Scheme     : $scheme
  Host       : $public
  Port       : $port
  Server key : $SERVER_KEY
  Health     : $base/healthcheck
  Socket     : $([ "$scheme" = https ] && echo wss || echo ws)://${base#*://}/ws
  Release    : $TANGO_RELEASE
EOF
chmod 600 "$DIR/connection.txt"
log "Deployed"
cat "$DIR/connection.txt"

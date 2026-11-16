#!/usr/bin/env bash
# Installs Docker Engine, the Compose plugin and curl if they are missing.
# Works on Ubuntu/Debian (apt) and Amazon Linux 2023 / RHEL-like (dnf/yum).
set -euo pipefail
[ "$(id -u)" = 0 ] || exec sudo -E bash "$0" "$@"

have_compose() { docker compose version >/dev/null 2>&1; }

if command -v docker >/dev/null 2>&1 && have_compose && command -v curl >/dev/null 2>&1; then
  systemctl enable --now docker >/dev/null 2>&1 || true
  echo "Docker $(docker --version | awk '{print $3}' | tr -d ,) and Compose $(docker compose version --short) already installed"
  exit 0
fi

echo "==> Installing Docker"
if command -v apt-get >/dev/null 2>&1; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get update -y
  apt-get install -y ca-certificates curl
  if ! command -v docker >/dev/null 2>&1; then
    apt-get install -y docker.io || (curl -fsSL https://get.docker.com | sh)
  fi
  if ! have_compose; then
    apt-get install -y docker-compose-v2 || apt-get install -y docker-compose-plugin || true
  fi
elif command -v dnf >/dev/null 2>&1 || command -v yum >/dev/null 2>&1; then
  pm=$(command -v dnf || command -v yum)
  $pm install -y docker curl-minimal 2>/dev/null || $pm install -y docker curl
else
  curl -fsSL https://get.docker.com | sh
fi

# Compose plugin from GitHub when the distribution has no package for it.
if ! have_compose; then
  arch=$(uname -m)
  case "$arch" in aarch64|arm64) arch=aarch64 ;; *) arch=x86_64 ;; esac
  mkdir -p /usr/local/lib/docker/cli-plugins
  curl -fsSL "https://github.com/docker/compose/releases/download/v2.32.4/docker-compose-linux-${arch}" \
    -o /usr/local/lib/docker/cli-plugins/docker-compose
  chmod +x /usr/local/lib/docker/cli-plugins/docker-compose
fi

systemctl enable --now docker
have_compose || { echo "Docker Compose could not be installed" >&2; exit 1; }
echo "Docker $(docker --version | awk '{print $3}' | tr -d ,) and Compose $(docker compose version --short) ready"

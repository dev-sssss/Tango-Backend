#!/bin/bash
# Runs on the server (started by the pipeline through AWS SSM).
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive HOME=/root ANSIBLE_FORCE_COLOR=0 ANSIBLE_NOCOLOR=1
cd "$(dirname "$0")"
cloud-init status --wait >/dev/null 2>&1 || true
if ! command -v ansible-playbook >/dev/null || ! python3 -c 'import boto3' 2>/dev/null; then
  apt-get update -y
  apt-get install -y ansible-core python3-boto3 curl
fi
ansible-playbook -c local -i localhost, site.yml -e @vars.json

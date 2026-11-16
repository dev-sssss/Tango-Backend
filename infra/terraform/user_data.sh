#!/bin/bash
# First boot only: the packages Ansible needs. Everything else is done by
# the Ansible playbook, run by the deploy pipeline through AWS SSM (which
# also installs these if this script could not, e.g. no network yet).
export DEBIAN_FRONTEND=noninteractive
for attempt in $(seq 1 30); do
  apt-get update -y && apt-get install -y ansible-core python3-boto3 curl ca-certificates && break
  sleep 10   # the Elastic IP is attached a few seconds after boot
done
systemctl enable --now snap.amazon-ssm-agent.amazon-ssm-agent.service || true

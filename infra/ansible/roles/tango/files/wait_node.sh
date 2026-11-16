#!/bin/bash
# wait_node.sh <admin_port> [seconds]: wait until a node answers its admin
# health check and is not draining.
port=$1; limit=${2:-90}
for ((i = 0; i < limit; i++)); do
  if out=$(curl -fsS "http://127.0.0.1:${port}/admin/health" 2>/dev/null) && echo "$out" | grep -q '"draining":false'; then
    echo "$out"; exit 0
  fi
  sleep 1
done
echo "node on admin port ${port} not healthy after ${limit}s" >&2
exit 1
